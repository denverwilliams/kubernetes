/*
Copyright 2014 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package gce

import (
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	gcfg "gopkg.in/gcfg.v1"

	"cloud.google.com/go/compute/metadata"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/flowcontrol"
	"k8s.io/kubernetes/pkg/cloudprovider"
	"k8s.io/kubernetes/pkg/controller"

	"github.com/golang/glog"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	cloudkms "google.golang.org/api/cloudkms/v1"
	computebeta "google.golang.org/api/compute/v0.beta"
	compute "google.golang.org/api/compute/v1"
	container "google.golang.org/api/container/v1"
)

const (
	ProviderName = "gce"

	k8sNodeRouteTag = "k8s-node-route"

	// AffinityTypeNone - no session affinity.
	gceAffinityTypeNone = "NONE"
	// AffinityTypeClientIP - affinity based on Client IP.
	gceAffinityTypeClientIP = "CLIENT_IP"
	// AffinityTypeClientIPProto - affinity based on Client IP and port.
	gceAffinityTypeClientIPProto = "CLIENT_IP_PROTO"

	operationPollInterval = 3 * time.Second
	// Creating Route in very large clusters, may take more than half an hour.
	operationPollTimeoutDuration = time.Hour

	// Each page can have 500 results, but we cap how many pages
	// are iterated through to prevent infinite loops if the API
	// were to continuously return a nextPageToken.
	maxPages = 25

	maxTargetPoolCreateInstances = 200

	// HTTP Load Balancer parameters
	// Configure 2 second period for external health checks.
	gceHcCheckIntervalSeconds = int64(2)
	gceHcTimeoutSeconds       = int64(1)
	// Start sending requests as soon as a pod is found on the node.
	gceHcHealthyThreshold = int64(1)
	// Defaults to 5 * 2 = 10 seconds before the LB will steer traffic away
	gceHcUnhealthyThreshold = int64(5)
)

// GCECloud is an implementation of Interface, LoadBalancer and Instances for Google Compute Engine.
type GCECloud struct {
	// ClusterID contains functionality for getting (and initializing) the ingress-uid. Call GCECloud.Initialize()
	// for the cloudprovider to start watching the configmap.
	ClusterID ClusterID

	service          *compute.Service
	serviceBeta      *computebeta.Service
	containerService *container.Service
	cloudkmsService  *cloudkms.Service
	clientBuilder    controller.ControllerClientBuilder
	projectID        string
	region           string
	localZone        string   // The zone in which we are running
	managedZones     []string // List of zones we are spanning (for multi-AZ clusters, primarily when running on master)
	networkURL       string
	subnetworkURL    string
	// Project which contains the cluster's network.
	// Used for specific network resources: firewalls, routes, and listing of zones in a region.
	networkProjectID         string
	onXPN                    bool
	nodeTags                 []string    // List of tags to use on firewall rules for load balancers
	lastComputedNodeTags     []string    // List of node tags calculated in GetHostTags()
	lastKnownNodeNames       sets.String // List of hostnames used to calculate lastComputedHostTags in GetHostTags(names)
	computeNodeTagLock       sync.Mutex  // Lock for computing and setting node tags
	nodeInstancePrefix       string      // If non-"", an advisory prefix for all nodes in the cluster
	useMetadataServer        bool
	operationPollRateLimiter flowcontrol.RateLimiter
	manager                  ServiceManager
	// sharedResourceLock is used to serialize GCE operations that may mutate shared state to
	// prevent inconsistencies. For example, load balancers manipulation methods will take the
	// lock to prevent shared resources from being prematurely deleted while the operation is
	// in progress.
	sharedResourceLock sync.Mutex
}

type ServiceManager interface {
	// Creates a new persistent disk on GCE with the given disk spec.
	CreateDisk(project string, zone string, disk *compute.Disk) (*compute.Operation, error)

	// Gets the persistent disk from GCE with the given diskName.
	GetDisk(project string, zone string, diskName string) (*compute.Disk, error)

	// Deletes the persistent disk from GCE with the given diskName.
	DeleteDisk(project string, zone string, disk string) (*compute.Operation, error)

	// Waits until GCE reports the given operation in the given zone as done.
	WaitForZoneOp(op *compute.Operation, zone string, mc *metricContext) error
}

type GCEServiceManager struct {
	gce *GCECloud
}

type Config struct {
	Global struct {
		TokenURL  string `gcfg:"token-url"`
		TokenBody string `gcfg:"token-body"`
		// ProjectID and NetworkProjectID can either be the numeric or string-based unique identifier that starts with [a-z]
		// However, both IDs need to be the same type for controllers to recognize this cluster as an XPN cluster.
		ProjectID          string   `gcfg:"project-id"`
		NetworkProjectID   string   `gcfg:"network-project-id"` // Project which contains the cluster's network. See networkProjectID in GCECloud
		NetworkName        string   `gcfg:"network-name"`
		SubnetworkName     string   `gcfg:"subnetwork-name"`
		NodeTags           []string `gcfg:"node-tags"`
		NodeInstancePrefix string   `gcfg:"node-instance-prefix"`
		Multizone          bool     `gcfg:"multizone"`
		ApiEndpoint        string   `gcfg:"api-endpoint"`
	}
}

func init() {
	cloudprovider.RegisterCloudProvider(
		ProviderName,
		func(config io.Reader) (cloudprovider.Interface, error) {
			return newGCECloud(config)
		})
}

// Raw access to the underlying GCE service, probably should only be used for e2e tests
func (g *GCECloud) GetComputeService() *compute.Service {
	return g.service
}

// Raw access to the cloudkmsService of GCE cloud. Required for encryption of etcd using Google KMS.
func (g *GCECloud) GetKMSService() *cloudkms.Service {
	return g.cloudkmsService
}

// newGCECloud creates a new instance of GCECloud.
func newGCECloud(config io.Reader) (*GCECloud, error) {
	apiEndpoint := ""

	// projectNumber is the numeric identifier. Note: there is also a unique string-based project identifier as well (see https://cloud.google.com/resource-manager/docs/creating-managing-projects#identifying_projects)
	projectNumber, zone, err := getProjectAndZone()
	if err != nil {
		return nil, err
	}
	// Default projectID to known project number
	projectID := projectNumber

	region, err := GetGCERegion(zone)
	if err != nil {
		return nil, err
	}

	// networkProjectNumber is a numeric identifier similar to the projectNumber above.
	networkProjectNumber, networkName, err := getNetworkProjectAndNameViaMetadata()
	if err != nil {
		return nil, err
	}
	// Default networkProjectID to known network project number
	networkProjectID := networkProjectNumber

	networkURL := gceNetworkURL(apiEndpoint, networkProjectNumber, networkName)
	subnetworkURL := ""

	// By default, Kubernetes clusters only run against one zone
	managedZones := []string{zone}

	tokenSource := google.ComputeTokenSource("")
	var nodeTags []string
	var nodeInstancePrefix string
	if config != nil {
		var cfg Config
		if err := gcfg.ReadInto(&cfg, config); err != nil {
			glog.Errorf("Couldn't read config: %v", err)
			return nil, err
		}
		glog.Infof("Using GCE provider config %+v", cfg)
		if cfg.Global.ApiEndpoint != "" {
			apiEndpoint = cfg.Global.ApiEndpoint
		}
		if cfg.Global.ProjectID != "" {
			projectID = cfg.Global.ProjectID
		}
		if cfg.Global.NetworkProjectID != "" {
			networkProjectID = cfg.Global.NetworkProjectID
		}

		if cfg.Global.NetworkName != "" {
			if strings.Contains(cfg.Global.NetworkName, "/") {
				otherNetworkProjectID, _ := getProjectIDInURL(cfg.Global.NetworkName)
				if networkProjectID != otherNetworkProjectID {
					glog.Warningf("Different network projects (may be id vs number). %q and %q", networkProjectID, otherNetworkProjectID)
				}

				networkURL = cfg.Global.NetworkName
			} else {
				networkURL = gceNetworkURL(apiEndpoint, networkProjectID, cfg.Global.NetworkName)
			}
		}

		if cfg.Global.SubnetworkName != "" {
			if strings.Contains(cfg.Global.SubnetworkName, "/") {
				subnetworkURL = cfg.Global.SubnetworkName
			} else {
				subnetworkURL = gceSubnetworkURL(apiEndpoint, networkProjectID, region, cfg.Global.SubnetworkName)
			}
		}

		if cfg.Global.TokenURL != "" {
			tokenSource = NewAltTokenSource(cfg.Global.TokenURL, cfg.Global.TokenBody)
		}
		nodeTags = cfg.Global.NodeTags
		nodeInstancePrefix = cfg.Global.NodeInstancePrefix
		if cfg.Global.Multizone {
			managedZones = nil // Use all zones in region
		}
	}

	return CreateGCECloud(apiEndpoint, projectID, networkProjectID, region, zone, managedZones, networkURL, subnetworkURL,
		nodeTags, nodeInstancePrefix, tokenSource, true /* useMetadataServer */)
}

// Creates a GCECloud object using the specified parameters.
// If no networkUrl is specified, loads networkName via rest call.
// If no tokenSource is specified, uses oauth2.DefaultTokenSource.
// If managedZones is nil / empty all zones in the region will be managed.
func CreateGCECloud(apiEndpoint, projectID, networkProjectID, region, zone string, managedZones []string, networkURL, subnetworkURL string, nodeTags []string,
	nodeInstancePrefix string, tokenSource oauth2.TokenSource, useMetadataServer bool) (*GCECloud, error) {

	// Determine if cluster is on shared VPC network
	// Must assert that the IDs are the same type (ID or number) before checking inequality
	onXPN := isProjectNumber(projectID) == isProjectNumber(networkProjectID) && projectID != networkProjectID

	client, err := newOauthClient(tokenSource)
	if err != nil {
		return nil, err
	}

	service, err := compute.New(client)
	if err != nil {
		return nil, err
	}

	if apiEndpoint != "" {
		service.BasePath = fmt.Sprintf("%sprojects/", apiEndpoint)
	}

	client, err = newOauthClient(tokenSource)
	serviceBeta, err := computebeta.New(client)
	if err != nil {
		return nil, err
	}

	containerService, err := container.New(client)
	if err != nil {
		return nil, err
	}

	cloudkmsService, err := cloudkms.New(client)
	if err != nil {
		return nil, err
	}

	if len(managedZones) == 0 {
		managedZones, err = getZonesForRegion(service, projectID, region)
		if err != nil {
			return nil, err
		}
	}
	if len(managedZones) != 1 {
		glog.Infof("managing multiple zones: %v", managedZones)
	}

	operationPollRateLimiter := flowcontrol.NewTokenBucketRateLimiter(10, 100) // 10 qps, 100 bucket size.

	gce := &GCECloud{
		service:                  service,
		serviceBeta:              serviceBeta,
		containerService:         containerService,
		cloudkmsService:          cloudkmsService,
		projectID:                projectID,
		networkProjectID:         networkProjectID,
		onXPN:                    onXPN,
		region:                   region,
		localZone:                zone,
		managedZones:             managedZones,
		networkURL:               networkURL,
		subnetworkURL:            subnetworkURL,
		nodeTags:                 nodeTags,
		nodeInstancePrefix:       nodeInstancePrefix,
		useMetadataServer:        useMetadataServer,
		operationPollRateLimiter: operationPollRateLimiter,
	}

	gce.manager = &GCEServiceManager{gce}
	return gce, nil
}

// Initialize takes in a clientBuilder and spawns a goroutine for watching the clusterid configmap.
// This must be called before utilizing the funcs of gce.ClusterID
func (gce *GCECloud) Initialize(clientBuilder controller.ControllerClientBuilder) {
	gce.clientBuilder = clientBuilder
	go gce.watchClusterID()
}

// LoadBalancer returns an implementation of LoadBalancer for Google Compute Engine.
func (gce *GCECloud) LoadBalancer() (cloudprovider.LoadBalancer, bool) {
	return gce, true
}

// Instances returns an implementation of Instances for Google Compute Engine.
func (gce *GCECloud) Instances() (cloudprovider.Instances, bool) {
	return gce, true
}

// Zones returns an implementation of Zones for Google Compute Engine.
func (gce *GCECloud) Zones() (cloudprovider.Zones, bool) {
	return gce, true
}

func (gce *GCECloud) Clusters() (cloudprovider.Clusters, bool) {
	return gce, true
}

// Routes returns an implementation of Routes for Google Compute Engine.
func (gce *GCECloud) Routes() (cloudprovider.Routes, bool) {
	return gce, true
}

// ProviderName returns the cloud provider ID.
func (gce *GCECloud) ProviderName() string {
	return ProviderName
}

// Region returns the region
func (gce *GCECloud) Region() string {
	return gce.region
}

// ProjectID returns the project ID which owns the instances
func (gce *GCECloud) ProjectID() string {
	return gce.projectID
}

// NetworkProjectID returns the project ID which owns the network
func (gce *GCECloud) NetworkProjectID() string {
	return gce.networkProjectID
}

// OnXPN returns true if the cluster is running on a shared VPC network
func (gce *GCECloud) OnXPN() bool {
	return gce.onXPN
}

// NetworkURL returns the network url
func (gce *GCECloud) NetworkURL() string {
	return gce.networkURL
}

// SubnetworkURL returns the subnetwork url
func (gce *GCECloud) SubnetworkURL() string {
	return gce.subnetworkURL
}

// Known-useless DNS search path.
var uselessDNSSearchRE = regexp.MustCompile(`^[0-9]+.google.internal.$`)

// ScrubDNS filters DNS settings for pods.
func (gce *GCECloud) ScrubDNS(nameservers, searches []string) (nsOut, srchOut []string) {
	// GCE has too many search paths by default. Filter the ones we know are useless.
	for _, s := range searches {
		if !uselessDNSSearchRE.MatchString(s) {
			srchOut = append(srchOut, s)
		}
	}
	return nameservers, srchOut
}

// GCECloud implements cloudprovider.Interface.
var _ cloudprovider.Interface = (*GCECloud)(nil)

func gceNetworkURL(apiEndpoint, project, network string) string {
	if apiEndpoint == "" {
		apiEndpoint = "https://www.googleapis.com/compute/v1/"
	}
	return fmt.Sprintf("%vprojects/%s/global/networks/%s", apiEndpoint, project, network)
}

func gceSubnetworkURL(apiEndpoint, project, region, subnetwork string) string {
	if apiEndpoint == "" {
		apiEndpoint = "https://www.googleapis.com/compute/v1/"
	}
	return fmt.Sprintf("%vprojects/%s/regions/%s/subnetworks/%s", apiEndpoint, project, region, subnetwork)
}

// Project IDs cannot have a digit for the first characeter. If the id contains a digit,
// then it must be a project number.
func isProjectNumber(idOrNumber string) bool {
	if len(idOrNumber) == 0 {
		return false
	}

	return idOrNumber[0] >= '0' && idOrNumber[0] <= '9'
}

// getProjectIDInURL parses typical full resource URLS and shorter URLS
// https://www.googleapis.com/compute/v1/projects/myproject/global/networks/mycustom
// projects/myproject/global/networks/mycustom
// All return "myproject"
func getProjectIDInURL(urlStr string) (string, error) {
	fields := strings.Split(urlStr, "/")
	for i, v := range fields {
		if v == "projects" && i < len(fields)-1 {
			return fields[i+1], nil
		}
	}
	return "", fmt.Errorf("could not find project field in url: %v", urlStr)
}

func getNetworkProjectAndNameViaMetadata() (string, string, error) {
	result, err := metadata.Get("instance/network-interfaces/0/network")
	if err != nil {
		return "", "", err
	}
	parts := strings.Split(result, "/")
	if len(parts) != 4 {
		return "", "", fmt.Errorf("unexpected response: %s", result)
	}
	return parts[1], parts[3], nil
}

func getZonesForRegion(svc *compute.Service, projectID, region string) ([]string, error) {
	// TODO: use PageToken to list all not just the first 500
	listCall := svc.Zones.List(projectID)

	// Filtering by region doesn't seem to work
	// (tested in https://cloud.google.com/compute/docs/reference/latest/zones/list)
	// listCall = listCall.Filter("region eq " + region)

	res, err := listCall.Do()
	if err != nil {
		return nil, fmt.Errorf("unexpected response listing zones: %v", err)
	}
	zones := []string{}
	for _, zone := range res.Items {
		regionName := lastComponent(zone.Region)
		if regionName == region {
			zones = append(zones, zone.Name)
		}
	}
	return zones, nil
}

func newOauthClient(tokenSource oauth2.TokenSource) (*http.Client, error) {
	if tokenSource == nil {
		var err error
		tokenSource, err = google.DefaultTokenSource(
			oauth2.NoContext,
			compute.CloudPlatformScope,
			compute.ComputeScope)
		glog.Infof("Using DefaultTokenSource %#v", tokenSource)
		if err != nil {
			return nil, err
		}
	} else {
		glog.Infof("Using existing Token Source %#v", tokenSource)
	}

	if err := wait.PollImmediate(5*time.Second, 30*time.Second, func() (bool, error) {
		if _, err := tokenSource.Token(); err != nil {
			glog.Errorf("error fetching initial token: %v", err)
			return false, nil
		}
		return true, nil
	}); err != nil {
		return nil, err
	}

	return oauth2.NewClient(oauth2.NoContext, tokenSource), nil
}

func (manager *GCEServiceManager) CreateDisk(
	project string,
	zone string,
	disk *compute.Disk) (*compute.Operation, error) {

	return manager.gce.service.Disks.Insert(project, zone, disk).Do()
}

func (manager *GCEServiceManager) GetDisk(
	project string,
	zone string,
	diskName string) (*compute.Disk, error) {

	return manager.gce.service.Disks.Get(project, zone, diskName).Do()
}

func (manager *GCEServiceManager) DeleteDisk(
	project string,
	zone string,
	diskName string) (*compute.Operation, error) {

	return manager.gce.service.Disks.Delete(project, zone, diskName).Do()
}

func (manager *GCEServiceManager) WaitForZoneOp(op *compute.Operation, zone string, mc *metricContext) error {
	return manager.gce.waitForZoneOp(op, zone, mc)
}
