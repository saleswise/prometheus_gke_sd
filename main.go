package main

import (
	"bytes"
	"encoding/base64"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"time"

	"golang.org/x/net/context"
	"gopkg.in/yaml.v2"

	google "golang.org/x/oauth2/google"
	compute "google.golang.org/api/compute/v1"
	container "google.golang.org/api/container/v1"
)

var (
	configFile = "/etc/gke-discoverer.yml"
)

func init() {
	flag.StringVar(&configFile, "config", configFile, "config file to use")
}

type Config struct {
	PrometheusConfigFile string `yaml:"prometheus_config"`
	CertificateStoreDir  string `yaml:"certificate_store"`
	PrometheusEndpoint   string `yaml:"prometheus_endpoint"`
	GCPProject           string `yaml:"gcp_project"`
	PollTime             int64  `yaml:"poll_time"`
}

type PrometheusConfig struct {
	ScrapeConfigs []ScrapeConfig         `yaml:"scrape_configs"`
	XXX           map[string]interface{} `yaml:",inline"`
}

type TLSConfig struct {
	CAFile   string `yaml:"ca_file,omitempty"`
	CertFile string `yaml:"cert_file,omitempty"`
	KeyFile  string `yaml:"key_file,omitempty"`
}
type BasicAuth struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

type KubeSDConfig struct {
	APIServers []string  `yaml:"api_servers"`
	Role       string    `yaml:"role"`
	InCluster  bool      `yaml:"in_cluster,omitempty"`
	TLSConfig  TLSConfig `yaml:"tls_config,omitempty"`
}

type ScrapeConfig struct {
	JobName             string          `yaml:"job_name"`
	KubernetesSDConfigs []KubeSDConfig  `yaml:"kubernetes_sd_configs,omitempty"`
	RelabelConfigs      []RelabelConfig `yaml:"relabel_configs,omitempty"`
	BasicAuth           `yaml:"basic_auth,omitempty"`
	XXX                 map[string]interface{} `yaml:",inline"`
}

func LoadConfig(filename string) Config {
	cfg := Config{}
	d, err := ioutil.ReadFile(filename)
	if err != nil {
		log.Fatal(err)
	}

	err = yaml.Unmarshal(d, &cfg)
	if err != nil {
		log.Fatal(err)
	}

	// Defaults
	if cfg.PrometheusConfigFile == "" {
		cfg.PrometheusConfigFile = "/etc/prometheus/prometheus.yml"
	}

	if cfg.CertificateStoreDir == "" {
		cfg.CertificateStoreDir = "/etc/prometheus/kube_sd_certs"
	}

	if cfg.PrometheusEndpoint == "" {
		cfg.PrometheusEndpoint = "http://localhost:9090"
	}

	if cfg.PollTime == 0 {
		cfg.PollTime = 30
	}

	if cfg.GCPProject == "" {
		log.Fatal("Please supply a GCP Project")
	}

	return cfg
}

func main() {
	flag.Parse()
	cfg := LoadConfig(configFile)

	// Create google gubbins.
	client, err := google.DefaultClient(context.TODO(), container.CloudPlatformScope, compute.ComputeReadonlyScope)
	if err != nil {
		log.Fatal(err)
	}

	containerSvc, err := container.New(client)
	if err != nil {
		log.Fatal(err)
	}

	computeSvc, err := compute.New(client)
	if err != nil {
		log.Fatal(err)
	}

	oldClusters := []container.Cluster{}
	hasChanged := false

	ticker := time.NewTicker(time.Duration(cfg.PollTime) * time.Second)

	for {
		select {
		case <-ticker.C:
			hasChanged = false
			res, err := computeSvc.Zones.List(cfg.GCPProject).Do()
			if err != nil {
				log.Fatal(err)
			}

			// Check every zone.
			newClusterList := []container.Cluster{}
			for _, z := range res.Items {

				fmt.Println("Zone : ", z.Name)
				res, err := containerSvc.Projects.Zones.Clusters.List(cfg.GCPProject, z.Name).Do()
				if err != nil {
					log.Fatal(err)
				}
				for _, c := range res.Clusters {
					newClusterList = append(newClusterList, *c)
				}
			}

			if len(oldClusters) == 0 {
				oldClusters = newClusterList
				hasChanged = true
			}

			indexesToRemove := []int{}

			// What entries are in the new cluster, but not in the old? (I.e New Entries)
			for _, cluster := range newClusterList {
				hasFound := false
				for _, ocluster := range oldClusters {
					if cluster.Name == ocluster.Name {
						hasFound = true
					}
				}
				if !hasFound {
					oldClusters = append(oldClusters, cluster)
					hasChanged = true
				}
			}

			// What needs to be cleaned up (i.e Clusters that have been deleted)
			for i, ocluster := range oldClusters {
				hasFound := false
				for _, cluster := range newClusterList {
					if cluster.Name == ocluster.Name {
						hasFound = true
					}
				}

				if !hasFound {
					indexesToRemove = append(indexesToRemove, i)
					hasChanged = true
				}
			}
			// Remove old entries
			for _, i := range indexesToRemove {
				oldClusters = oldClusters[:i+copy(oldClusters[i:], oldClusters[i+1:])]
			}

			if !hasChanged {
				fmt.Println("No Difference in config")
				break
			}
			fmt.Println("Detected Changed in Config:", len(oldClusters), len(newClusterList))
			fmt.Println("Old Clusters:")
			for _, c := range oldClusters {
				fmt.Println(c.Name)
			}
			fmt.Println("New Clusters:")
			for _, c := range newClusterList {
				fmt.Println(c.Name)
			}

			newScrapeConfigs := []ScrapeConfig{}

			for _, cluster := range oldClusters {
				CAFile := fmt.Sprintf("%v/%v-ca.pem", cfg.CertificateStoreDir, cluster.Name)
				CertFile := fmt.Sprintf("%v/%v-cert.pem", cfg.CertificateStoreDir, cluster.Name)
				KeyFile := fmt.Sprintf("%v/%v-key.pem", cfg.CertificateStoreDir, cluster.Name)

				decodedCA, err := base64.StdEncoding.DecodeString(cluster.MasterAuth.ClusterCaCertificate)
				if err != nil {
					log.Fatal(err)
				}
				decodedCert, err := base64.StdEncoding.DecodeString(cluster.MasterAuth.ClientCertificate)
				if err != nil {
					log.Fatal(err)
				}
				decodedKey, err := base64.StdEncoding.DecodeString(cluster.MasterAuth.ClientKey)
				if err != nil {
					log.Fatal(err)
				}

				err = ioutil.WriteFile(CAFile, decodedCA, 0644)
				if err != nil {
					log.Fatal(err)
				}

				err = ioutil.WriteFile(CertFile, decodedCert, 0644)
				if err != nil {
					log.Fatal(err)
				}

				err = ioutil.WriteFile(KeyFile, decodedKey, 0644)
				if err != nil {
					log.Fatal(err)
				}

				for r, c := range GetRoles() {
					scc := ScrapeConfig{
						JobName: fmt.Sprintf("kubernetes_%v_%v", cluster.Name, r),
						BasicAuth: BasicAuth{
							Username: cluster.MasterAuth.Username,
							Password: cluster.MasterAuth.Password,
						},
						KubernetesSDConfigs: []KubeSDConfig{
							KubeSDConfig{
								APIServers: []string{
									"https://" + cluster.Endpoint,
								},
								Role:      r,
								InCluster: false,
								TLSConfig: TLSConfig{
									CAFile:   CAFile,
									CertFile: CertFile,
									KeyFile:  KeyFile,
								},
							},
						},
						RelabelConfigs: c,
					}

					newScrapeConfigs = append(newScrapeConfigs, scc)
				}
			}

			cfgp := PrometheusConfig{}

			d, err := ioutil.ReadFile(cfg.PrometheusConfigFile)
			if err != nil {
				log.Fatal(err)
			}

			err = yaml.Unmarshal(d, &cfgp)
			if err != nil {
				log.Fatal(err)
			}

			for _, sc := range cfgp.ScrapeConfigs {
				// I.e we're not a kube scrape config, and we were in the original file - lets leave this alone
				if len(sc.KubernetesSDConfigs) == 0 {
					newScrapeConfigs = append(newScrapeConfigs, sc)
				}
			}

			cfgp.ScrapeConfigs = newScrapeConfigs

			d, err = yaml.Marshal(&cfgp)
			if err != nil {
				log.Fatal(err)
			}

			err = ioutil.WriteFile(cfg.PrometheusConfigFile, d, 0644)
			if err != nil {
				log.Fatal(err)
			}

			fmt.Println("Reloading Prometheus Config")
			// Reload Prometheus Config
			_, err = http.Post(cfg.PrometheusEndpoint+"/-/reload", "text/plain", bytes.NewBufferString(""))
			if err != nil {
				log.Fatal(err)
			}
		}
	}
}