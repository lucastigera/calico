// Copyright (c) 2016-2025 Tigera, Inc. All rights reserved.

// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package k8s

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"

	apiv3 "github.com/projectcalico/api/pkg/apis/projectcalico/v3"
	log "github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth" // Import all auth providers.
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	adminpolicyclient "sigs.k8s.io/network-policy-api/pkg/client/clientset/versioned/typed/apis/v1alpha1"

	"github.com/projectcalico/calico/libcalico-go/lib/apiconfig"
	libapiv3 "github.com/projectcalico/calico/libcalico-go/lib/apis/v3"
	"github.com/projectcalico/calico/libcalico-go/lib/backend/api"
	"github.com/projectcalico/calico/libcalico-go/lib/backend/k8s/conversion"
	"github.com/projectcalico/calico/libcalico-go/lib/backend/k8s/resources"
	calischeme "github.com/projectcalico/calico/libcalico-go/lib/backend/k8s/scheme"
	"github.com/projectcalico/calico/libcalico-go/lib/backend/model"
	cerrors "github.com/projectcalico/calico/libcalico-go/lib/errors"
	"github.com/projectcalico/calico/libcalico-go/lib/net"
	"github.com/projectcalico/calico/libcalico-go/lib/winutils"
)

var (
	resourceKeyType  = reflect.TypeOf(model.ResourceKey{})
	resourceListType = reflect.TypeOf(model.ResourceListOptions{})
)

type KubeClient struct {
	// Main Kubernetes clients.
	ClientSet *kubernetes.Clientset

	// Client for interacting with CustomResourceDefinition.
	crdClientV1 *rest.RESTClient

	// Client for interacting with K8S Admin Network Policy, and BaselineAdminNetworkPolicy.
	k8sAdminPolicyClient *adminpolicyclient.PolicyV1alpha1Client

	disableNodePoll bool

	// Contains methods for converting Kubernetes resources to
	// Calico resources.
	converter conversion.Converter

	// Resource clients keyed off Kind.
	clientsByResourceKind map[string]resources.K8sResourceClient

	// Non v3 resource clients keyed off Key Type.
	clientsByKeyType map[reflect.Type]resources.K8sResourceClient

	// Non v3 resource clients keyed off List Type.
	clientsByListType map[reflect.Type]resources.K8sResourceClient
}

func NewKubeClient(ca *apiconfig.CalicoAPIConfigSpec) (api.Client, error) {
	config, cs, err := CreateKubernetesClientset(ca)
	if err != nil {
		return nil, err
	}

	crdClientV1, err := buildCRDClientV1(*config)
	if err != nil {
		return nil, fmt.Errorf("Failed to build V1 CRD client: %v", err)
	}

	k8sAdminPolicyClient, err := buildK8SAdminPolicyClient(config)
	if err != nil {
		return nil, fmt.Errorf("Failed to build K8S Admin Network Policy client: %v", err)
	}

	kubeClient := &KubeClient{
		ClientSet:             cs,
		crdClientV1:           crdClientV1,
		k8sAdminPolicyClient:  k8sAdminPolicyClient,
		disableNodePoll:       ca.K8sDisableNodePoll,
		clientsByResourceKind: make(map[string]resources.K8sResourceClient),
		clientsByKeyType:      make(map[reflect.Type]resources.K8sResourceClient),
		clientsByListType:     make(map[reflect.Type]resources.K8sResourceClient),
	}

	// Create the Calico sub-clients and register them.
	kubeClient.registerResourceClient(
		reflect.TypeOf(model.ResourceKey{}),
		reflect.TypeOf(model.ResourceListOptions{}),
		apiv3.KindIPPool,
		resources.NewIPPoolClient(cs, crdClientV1),
	)
	kubeClient.registerResourceClient(
		reflect.TypeOf(model.ResourceKey{}),
		reflect.TypeOf(model.ResourceListOptions{}),
		apiv3.KindIPReservation,
		resources.NewIPReservationClient(cs, crdClientV1),
	)
	kubeClient.registerResourceClient(
		reflect.TypeOf(model.ResourceKey{}),
		reflect.TypeOf(model.ResourceListOptions{}),
		apiv3.KindGlobalNetworkPolicy,
		resources.NewGlobalNetworkPolicyClient(cs, crdClientV1),
	)
	kubeClient.registerResourceClient(
		reflect.TypeOf(model.ResourceKey{}),
		reflect.TypeOf(model.ResourceListOptions{}),
		apiv3.KindStagedGlobalNetworkPolicy,
		resources.NewStagedGlobalNetworkPolicyClient(cs, crdClientV1),
	)
	kubeClient.registerResourceClient(
		reflect.TypeOf(model.ResourceKey{}),
		reflect.TypeOf(model.ResourceListOptions{}),
		model.KindKubernetesAdminNetworkPolicy,
		resources.NewKubernetesAdminNetworkPolicyClient(k8sAdminPolicyClient),
	)
	kubeClient.registerResourceClient(
		reflect.TypeOf(model.ResourceKey{}),
		reflect.TypeOf(model.ResourceListOptions{}),
		model.KindKubernetesBaselineAdminNetworkPolicy,
		resources.NewKubernetesBaselineAdminNetworkPolicyClient(k8sAdminPolicyClient),
	)
	kubeClient.registerResourceClient(
		reflect.TypeOf(model.ResourceKey{}),
		reflect.TypeOf(model.ResourceListOptions{}),
		apiv3.KindGlobalNetworkSet,
		resources.NewGlobalNetworkSetClient(cs, crdClientV1),
	)
	kubeClient.registerResourceClient(
		reflect.TypeOf(model.ResourceKey{}),
		reflect.TypeOf(model.ResourceListOptions{}),
		apiv3.KindNetworkPolicy,
		resources.NewNetworkPolicyClient(cs, crdClientV1),
	)
	kubeClient.registerResourceClient(
		reflect.TypeOf(model.ResourceKey{}),
		reflect.TypeOf(model.ResourceListOptions{}),
		apiv3.KindStagedNetworkPolicy,
		resources.NewStagedNetworkPolicyClient(cs, crdClientV1),
	)
	kubeClient.registerResourceClient(
		reflect.TypeOf(model.ResourceKey{}),
		reflect.TypeOf(model.ResourceListOptions{}),
		model.KindKubernetesNetworkPolicy,
		resources.NewKubernetesNetworkPolicyClient(cs),
	)
	kubeClient.registerResourceClient(
		reflect.TypeOf(model.ResourceKey{}),
		reflect.TypeOf(model.ResourceListOptions{}),
		apiv3.KindStagedKubernetesNetworkPolicy,
		resources.NewStagedKubernetesNetworkPolicyClient(cs, crdClientV1),
	)
	kubeClient.registerResourceClient(
		reflect.TypeOf(model.ResourceKey{}),
		reflect.TypeOf(model.ResourceListOptions{}),
		model.KindKubernetesEndpointSlice,
		resources.NewKubernetesEndpointSliceClient(cs),
	)
	kubeClient.registerResourceClient(
		reflect.TypeOf(model.ResourceKey{}),
		reflect.TypeOf(model.ResourceListOptions{}),
		apiv3.KindNetworkSet,
		resources.NewNetworkSetClient(cs, crdClientV1),
	)
	kubeClient.registerResourceClient(
		reflect.TypeOf(model.ResourceKey{}),
		reflect.TypeOf(model.ResourceListOptions{}),
		apiv3.KindTier,
		resources.NewTierClient(cs, crdClientV1),
	)
	kubeClient.registerResourceClient(
		reflect.TypeOf(model.ResourceKey{}),
		reflect.TypeOf(model.ResourceListOptions{}),
		apiv3.KindBGPPeer,
		resources.NewBGPPeerClient(cs, crdClientV1),
	)
	kubeClient.registerResourceClient(
		reflect.TypeOf(model.ResourceKey{}),
		reflect.TypeOf(model.ResourceListOptions{}),
		apiv3.KindBGPConfiguration,
		resources.NewBGPConfigClient(cs, crdClientV1),
	)
	kubeClient.registerResourceClient(
		reflect.TypeOf(model.ResourceKey{}),
		reflect.TypeOf(model.ResourceListOptions{}),
		apiv3.KindFelixConfiguration,
		resources.NewFelixConfigClient(cs, crdClientV1),
	)
	kubeClient.registerResourceClient(
		reflect.TypeOf(model.ResourceKey{}),
		reflect.TypeOf(model.ResourceListOptions{}),
		apiv3.KindClusterInformation,
		resources.NewClusterInfoClient(cs, crdClientV1),
	)
	kubeClient.registerResourceClient(
		reflect.TypeOf(model.ResourceKey{}),
		reflect.TypeOf(model.ResourceListOptions{}),
		libapiv3.KindNode,
		resources.NewNodeClient(cs, ca.K8sUsePodCIDR),
	)
	kubeClient.registerResourceClient(
		reflect.TypeOf(model.ResourceKey{}),
		reflect.TypeOf(model.ResourceListOptions{}),
		apiv3.KindProfile,
		resources.NewProfileClient(cs),
	)
	kubeClient.registerResourceClient(
		reflect.TypeOf(model.ResourceKey{}),
		reflect.TypeOf(model.ResourceListOptions{}),
		apiv3.KindHostEndpoint,
		resources.NewHostEndpointClient(cs, crdClientV1),
	)
	kubeClient.registerResourceClient(
		reflect.TypeOf(model.ResourceKey{}),
		reflect.TypeOf(model.ResourceListOptions{}),
		libapiv3.KindWorkloadEndpoint,
		resources.NewWorkloadEndpointClient(cs),
	)
	kubeClient.registerResourceClient(
		reflect.TypeOf(model.ResourceKey{}),
		reflect.TypeOf(model.ResourceListOptions{}),
		apiv3.KindKubeControllersConfiguration,
		resources.NewKubeControllersConfigClient(cs, crdClientV1),
	)
	kubeClient.registerResourceClient(
		reflect.TypeOf(model.ResourceKey{}),
		reflect.TypeOf(model.ResourceListOptions{}),
		apiv3.KindCalicoNodeStatus,
		resources.NewCalicoNodeStatusClient(cs, crdClientV1),
	)
	kubeClient.registerResourceClient(
		reflect.TypeOf(model.ResourceKey{}),
		reflect.TypeOf(model.ResourceListOptions{}),
		model.KindKubernetesService,
		resources.NewServiceClient(cs),
	)
	kubeClient.registerResourceClient(
		reflect.TypeOf(model.ResourceKey{}),
		reflect.TypeOf(model.ResourceListOptions{}),
		libapiv3.KindIPAMConfig,
		resources.NewIPAMConfigClient(cs, crdClientV1),
	)
	kubeClient.registerResourceClient(
		reflect.TypeOf(model.ResourceKey{}),
		reflect.TypeOf(model.ResourceListOptions{}),
		libapiv3.KindBlockAffinity,
		resources.NewBlockAffinityClient(cs, crdClientV1),
	)
	kubeClient.registerResourceClient(
		reflect.TypeOf(model.ResourceKey{}),
		reflect.TypeOf(model.ResourceListOptions{}),
		apiv3.KindBGPFilter,
		resources.NewBGPFilterClient(cs, crdClientV1),
	)

	if !ca.K8sUsePodCIDR {
		// Using Calico IPAM - use CRDs to back IPAM resources.
		log.Debug("Calico is configured to use calico-ipam")
		kubeClient.registerResourceClient(
			reflect.TypeOf(model.BlockAffinityKey{}),
			reflect.TypeOf(model.BlockAffinityListOptions{}),
			libapiv3.KindBlockAffinity,
			resources.NewBlockAffinityClient(cs, crdClientV1),
		)
		kubeClient.registerResourceClient(
			reflect.TypeOf(model.BlockKey{}),
			reflect.TypeOf(model.BlockListOptions{}),
			libapiv3.KindIPAMBlock,
			resources.NewIPAMBlockClient(cs, crdClientV1),
		)
		kubeClient.registerResourceClient(
			reflect.TypeOf(model.IPAMHandleKey{}),
			reflect.TypeOf(model.IPAMHandleListOptions{}),
			libapiv3.KindIPAMHandle,
			resources.NewIPAMHandleClient(cs, crdClientV1),
		)
		kubeClient.registerResourceClient(
			reflect.TypeOf(model.IPAMConfigKey{}),
			nil,
			libapiv3.KindIPAMConfig,
			resources.NewIPAMConfigClient(cs, crdClientV1),
		)
	}

	return kubeClient, nil
}

// deduplicate removes any duplicated values and returns a new slice, keeping the order unchanged
//
//	based on deduplicate([]string) []string found in k8s.io/client-go/tools/clientcmd/loader.go#634
//	Copyright 2014 The Kubernetes Authors.
func deduplicate(s []string) []string {
	encountered := map[string]struct{}{}
	ret := make([]string, 0)
	for i := range s {
		if _, ok := encountered[s[i]]; ok {
			continue
		}
		encountered[s[i]] = struct{}{}
		ret = append(ret, s[i])
	}
	return ret
}

// fill out loading rules based on filename(s) encountered in specified kubeconfig
func fillLoadingRulesFromKubeConfigSpec(loadingRules *clientcmd.ClientConfigLoadingRules, kubeConfig string) {
	fileList := filepath.SplitList(kubeConfig)

	if len(fileList) > 1 {
		loadingRules.Precedence = deduplicate(fileList)
		loadingRules.WarnIfAllMissing = true
		return
	}

	loadingRules.ExplicitPath = kubeConfig
}

func CreateKubernetesClientset(ca *apiconfig.CalicoAPIConfigSpec) (*rest.Config, *kubernetes.Clientset, error) {
	// Use the kubernetes client code to load the kubeconfig file and combine it with the overrides.
	configOverrides := &clientcmd.ConfigOverrides{}
	overridesMap := []struct {
		variable *string
		value    string
	}{
		{&configOverrides.CurrentContext, ca.K8sCurrentContext},
		{&configOverrides.ClusterInfo.Server, ca.K8sAPIEndpoint},
		{&configOverrides.AuthInfo.ClientCertificate, ca.K8sCertFile},
		{&configOverrides.AuthInfo.ClientKey, ca.K8sKeyFile},
		{&configOverrides.ClusterInfo.CertificateAuthority, ca.K8sCAFile},
		{&configOverrides.AuthInfo.Token, ca.K8sAPIToken},
	}

	// Set an explicit path to the kubeconfig if one
	// was provided.
	loadingRules := clientcmd.ClientConfigLoadingRules{}
	if ca.Kubeconfig != "" {
		fillLoadingRulesFromKubeConfigSpec(&loadingRules, ca.Kubeconfig)
	}

	// Using the override map above, populate any non-empty values.
	for _, override := range overridesMap {
		if override.value != "" {
			*override.variable = override.value
		}
	}
	if ca.K8sInsecureSkipTLSVerify {
		configOverrides.ClusterInfo.InsecureSkipTLSVerify = true
	}

	// A kubeconfig file was provided.  Use it to load a config, passing through
	// any overrides.
	var config *rest.Config
	var err error
	if ca.KubeconfigInline != "" {
		var clientConfig clientcmd.ClientConfig
		clientConfig, err = clientcmd.NewClientConfigFromBytes([]byte(ca.KubeconfigInline))
		if err != nil {
			return nil, nil, resources.K8sErrorToCalico(err, nil)
		}
		config, err = clientConfig.ClientConfig()
	} else {
		config, err = winutils.NewNonInteractiveDeferredLoadingClientConfig(
			&loadingRules, configOverrides)
	}
	if err != nil {
		return nil, nil, resources.K8sErrorToCalico(err, nil)
	}

	config.AcceptContentTypes = strings.Join([]string{runtime.ContentTypeProtobuf, runtime.ContentTypeJSON}, ",")
	config.ContentType = runtime.ContentTypeProtobuf

	// Overwrite the QPS if provided. Default QPS is 5.
	if ca.K8sClientQPS != float32(0) {
		config.QPS = ca.K8sClientQPS
	}

	// Create the clientset. We increase the burst so that the IPAM code performs
	// efficiently. The IPAM code can create bursts of requests to the API, so
	// in order to keep pod creation times sensible we allow a higher request rate.
	config.Burst = 100
	cs, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, nil, resources.K8sErrorToCalico(err, nil)
	}
	return config, cs, nil
}

// registerResourceClient registers a specific resource client with the associated
// key and list types (and for v3 resources with the resource kind - since these share
// a common key and list type).
func (c *KubeClient) registerResourceClient(keyType, listType reflect.Type, resourceKind string, client resources.K8sResourceClient) {
	if keyType == resourceKeyType {
		c.clientsByResourceKind[resourceKind] = client
	} else {
		c.clientsByKeyType[keyType] = client
		c.clientsByListType[listType] = client
	}
}

// getResourceClientFromKey returns the appropriate resource client for the v3 resource kind.
func (c *KubeClient) GetResourceClientFromResourceKind(kind string) resources.K8sResourceClient {
	return c.clientsByResourceKind[kind]
}

// getResourceClientFromKey returns the appropriate resource client for the key.
func (c *KubeClient) getResourceClientFromKey(key model.Key) resources.K8sResourceClient {
	kt := reflect.TypeOf(key)
	if kt == resourceKeyType {
		return c.clientsByResourceKind[key.(model.ResourceKey).Kind]
	} else {
		return c.clientsByKeyType[kt]
	}
}

// getResourceClientFromList returns the appropriate resource client for the list.
func (c *KubeClient) getResourceClientFromList(list model.ListInterface) resources.K8sResourceClient {
	lt := reflect.TypeOf(list)
	if lt == resourceListType {
		return c.clientsByResourceKind[list.(model.ResourceListOptions).Kind]
	} else {
		return c.clientsByListType[lt]
	}
}

// EnsureInitialized checks that the necessary custom resource definitions
// exist in the backend. This usually passes when using etcd
// as a backend but can often fail when using KDD as it relies
// on various custom resources existing.
// To ensure the datastore is initialized, this function checks that a
// known custom resource is defined: GlobalFelixConfig. It accomplishes this
// by trying to set the ClusterType (an instance of GlobalFelixConfig).
func (c *KubeClient) EnsureInitialized() error {
	return nil
}

// Remove Calico-creatable data from the datastore.  This is purely used for the
// test framework.
func (c *KubeClient) Clean() error {
	log.Warning("Cleaning KDD of all Calico-creatable data")
	kinds := []string{
		apiv3.KindBGPConfiguration,
		apiv3.KindBGPPeer,
		apiv3.KindClusterInformation,
		apiv3.KindCalicoNodeStatus,
		apiv3.KindFelixConfiguration,
		apiv3.KindGlobalNetworkPolicy,
		apiv3.KindStagedGlobalNetworkPolicy,
		apiv3.KindNetworkPolicy,
		apiv3.KindStagedNetworkPolicy,
		apiv3.KindStagedKubernetesNetworkPolicy,
		apiv3.KindTier,
		apiv3.KindGlobalNetworkSet,
		apiv3.KindNetworkSet,
		apiv3.KindIPPool,
		apiv3.KindIPReservation,
		apiv3.KindHostEndpoint,
		apiv3.KindKubeControllersConfiguration,
		libapiv3.KindIPAMConfig,
		libapiv3.KindBlockAffinity,
		apiv3.KindBGPFilter,
	}
	ctx := context.Background()
	for _, k := range kinds {
		lo := model.ResourceListOptions{Kind: k}
		if rs, err := c.List(ctx, lo, ""); err != nil {
			log.WithError(err).WithField("Kind", k).Warning("Failed to list resources")
		} else {
			for _, r := range rs.KVPairs {
				if _, err = c.Delete(ctx, r.Key, r.Revision); err != nil {
					log.WithField("Key", r.Key).Warning("Failed to delete entry from KDD")
				}
			}
		}
	}

	// Cleanup IPAM resources that have slightly different backend semantics.
	for _, li := range []model.ListInterface{
		model.BlockListOptions{},
		model.BlockAffinityListOptions{},
		model.BlockAffinityListOptions{},
		model.IPAMHandleListOptions{},
	} {
		if rs, err := c.List(ctx, li, ""); err != nil {
			log.WithError(err).WithField("Kind", li).Warning("Failed to list resources")
		} else {
			for _, r := range rs.KVPairs {
				if _, err = c.DeleteKVP(ctx, r); err != nil {
					log.WithError(err).WithField("Key", r.Key).Warning("Failed to delete entry from KDD")
				}
			}
		}
	}

	// Get a list of Nodes and remove all BGP configuration from the nodes.
	if nodes, err := c.List(ctx, model.ResourceListOptions{Kind: libapiv3.KindNode}, ""); err != nil {
		log.Warning("Failed to list Nodes")
	} else {
		for _, nodeKvp := range nodes.KVPairs {
			node := nodeKvp.Value.(*libapiv3.Node)
			node.Spec.BGP = nil
			if _, err := c.Update(ctx, nodeKvp); err != nil {
				log.WithField("Node", node.Name).Warning("Failed to remove Calico config from node")
			}
		}
	}

	// Delete global IPAM config
	if _, err := c.Delete(ctx, model.IPAMConfigKey{}, ""); err != nil {
		log.WithError(err).WithField("key", model.IPAMConfigGlobalName).Warning("Failed to delete global IPAM Config from KDD")
	}
	return nil
}

// Close the underlying client
func (c *KubeClient) Close() error {
	log.Debugf("Closing client - NOOP")
	return nil
}

// buildK8SAdminPolicyClient builds a RESTClient configured to interact (Baseline) Admin Network Policy.
func buildK8SAdminPolicyClient(cfg *rest.Config) (*adminpolicyclient.PolicyV1alpha1Client, error) {
	return adminpolicyclient.NewForConfig(cfg)
}

// buildCRDClientV1 builds a RESTClient configured to interact with Calico CustomResourceDefinitions
func buildCRDClientV1(cfg rest.Config) (*rest.RESTClient, error) {
	// Generate config using the base config.
	cfg.GroupVersion = &schema.GroupVersion{
		Group:   "crd.projectcalico.org",
		Version: "v1",
	}
	cfg.APIPath = "/apis"
	cfg.ContentType = runtime.ContentTypeJSON
	cfg.NegotiatedSerializer = serializer.WithoutConversionCodecFactory{CodecFactory: scheme.Codecs}

	cli, err := rest.RESTClientFor(&cfg)
	if err != nil {
		return nil, err
	}

	calischeme.AddCalicoResourcesToScheme()

	return cli, nil
}

// Create an entry in the datastore.  This errors if the entry already exists.
func (c *KubeClient) Create(ctx context.Context, d *model.KVPair) (*model.KVPair, error) {
	log.Debugf("Performing 'Create' for %+v", d)
	client := c.getResourceClientFromKey(d.Key)
	if client == nil {
		log.Debug("Attempt to 'Create' using kubernetes backend is not supported.")
		return nil, cerrors.ErrorOperationNotSupported{
			Identifier: d.Key,
			Operation:  "Create",
		}
	}
	return client.Create(ctx, d)
}

// Update an existing entry in the datastore.  This errors if the entry does
// not exist.
func (c *KubeClient) Update(ctx context.Context, d *model.KVPair) (*model.KVPair, error) {
	log.Debugf("Performing 'Update' for %+v", d)
	client := c.getResourceClientFromKey(d.Key)
	if client == nil {
		log.Debug("Attempt to 'Update' using kubernetes backend is not supported.")
		return nil, cerrors.ErrorOperationNotSupported{
			Identifier: d.Key,
			Operation:  "Update",
		}
	}
	return client.Update(ctx, d)
}

// Set an existing entry in the datastore.  This ignores whether an entry already
// exists.  This is not exposed in the main client - but we keep here for the backend
// API.
func (c *KubeClient) Apply(ctx context.Context, kvp *model.KVPair) (*model.KVPair, error) {
	logContext := log.WithFields(log.Fields{
		"Key":   kvp.Key,
		"Value": kvp.Value,
	})
	logContext.Debug("Apply Kubernetes resource")

	// Attempt to Create and do an Update if the resource already exists.
	// We only log debug here since the Create and Update will also log.
	// Can't set Revision while creating a resource.
	updated, err := c.Create(ctx, &model.KVPair{
		Key:   kvp.Key,
		Value: kvp.Value,
	})
	if err != nil {
		if _, ok := err.(cerrors.ErrorResourceAlreadyExists); !ok {
			logContext.Debug("Error applying resource (using Create)")
			return nil, err
		}

		// Try to Update if the resource already exists.
		updated, err = c.Update(ctx, kvp)
		if err != nil {
			logContext.Debug("Error applying resource (using Update)")
			return nil, err
		}
	}
	return updated, nil
}

// Delete an entry in the datastore.
func (c *KubeClient) DeleteKVP(ctx context.Context, kvp *model.KVPair) (*model.KVPair, error) {
	log.Debugf("Performing 'DeleteKVP' for %+v", kvp.Key)
	client := c.getResourceClientFromKey(kvp.Key)
	if client == nil {
		log.Debug("Attempt to 'DeleteKVP' using kubernetes backend is not supported.")
		return nil, cerrors.ErrorOperationNotSupported{
			Identifier: kvp.Key,
			Operation:  "Delete",
		}
	}
	return client.DeleteKVP(ctx, kvp)
}

// Delete an entry in the datastore by key.
func (c *KubeClient) Delete(ctx context.Context, k model.Key, revision string) (*model.KVPair, error) {
	log.Debugf("Performing 'Delete' for %+v", k)
	client := c.getResourceClientFromKey(k)
	if client == nil {
		log.Debug("Attempt to 'Delete' using kubernetes backend is not supported.")
		return nil, cerrors.ErrorOperationNotSupported{
			Identifier: k,
			Operation:  "Delete",
		}
	}
	return client.Delete(ctx, k, revision, nil)
}

// Get an entry from the datastore.  This errors if the entry does not exist.
func (c *KubeClient) Get(ctx context.Context, k model.Key, revision string) (*model.KVPair, error) {
	log.Debugf("Performing 'Get' for %+v %v", k, revision)
	client := c.getResourceClientFromKey(k)
	if client == nil {
		log.Debug("Attempt to 'Get' using kubernetes backend is not supported.")
		return nil, cerrors.ErrorOperationNotSupported{
			Identifier: k,
			Operation:  "Get",
		}
	}
	return client.Get(ctx, k, revision)
}

// List entries in the datastore.  This may return an empty list if there are
// no entries matching the request in the ListInterface.
func (c *KubeClient) List(ctx context.Context, l model.ListInterface, revision string) (*model.KVPairList, error) {
	log.Debugf("Performing 'List' for %+v %v", l, reflect.TypeOf(l))
	client := c.getResourceClientFromList(l)
	if client == nil {
		log.Info("Attempt to 'List' using kubernetes backend is not supported.")
		return nil, cerrors.ErrorOperationNotSupported{
			Identifier: l,
			Operation:  "List",
		}
	}
	return client.List(ctx, l, revision)
}

// Watch starts a watch on a particular resource type.
func (c *KubeClient) Watch(ctx context.Context, l model.ListInterface, options api.WatchOptions) (api.WatchInterface, error) {
	log.Debugf("Performing 'Watch' for %+v %v", l, reflect.TypeOf(l))
	client := c.getResourceClientFromList(l)
	if client == nil {
		log.Debug("Attempt to 'Watch' using kubernetes backend is not supported.")
		return nil, cerrors.ErrorOperationNotSupported{
			Identifier: l,
			Operation:  "Watch",
		}
	}
	return client.Watch(ctx, l, options)
}

func (c *KubeClient) getReadyStatus(ctx context.Context, k model.ReadyFlagKey, revision string) (*model.KVPair, error) {
	return &model.KVPair{Key: k, Value: true}, nil
}

func (c *KubeClient) listHostConfig(ctx context.Context, l model.HostConfigListOptions, revision string) (*model.KVPairList, error) {
	kvps := []*model.KVPair{}

	// Short circuit if they aren't asking for information we can provide.
	if l.Name != "" && l.Name != "IpInIpTunnelAddr" {
		return &model.KVPairList{
			KVPairs:  kvps,
			Revision: revision,
		}, nil
	}

	// First see if we were handed a specific host, if not list all Nodes
	if l.Hostname == "" {
		nodes, err := c.ClientSet.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, resources.K8sErrorToCalico(err, l)
		}

		for _, node := range nodes.Items {
			kvp, err := getTunIp(&node)
			if err != nil || kvp == nil {
				continue
			}

			kvps = append(kvps, kvp)
		}
	} else {
		node, err := c.ClientSet.CoreV1().Nodes().Get(ctx, l.Hostname, metav1.GetOptions{})
		if err != nil {
			return nil, resources.K8sErrorToCalico(err, l)
		}

		kvp, err := getTunIp(node)
		if err != nil || kvp == nil {
			return &model.KVPairList{
				KVPairs:  []*model.KVPair{},
				Revision: revision,
			}, nil
		}

		kvps = append(kvps, kvp)
	}

	return &model.KVPairList{
		KVPairs:  kvps,
		Revision: revision,
	}, nil
}

func getTunIp(n *v1.Node) (*model.KVPair, error) {
	if n.Spec.PodCIDR == "" {
		log.Warnf("Node %s does not have podCIDR for HostConfig", n.Name)
		return nil, nil
	}

	ip, _, err := net.ParseCIDR(n.Spec.PodCIDR)
	if err != nil {
		log.Warnf("Invalid podCIDR for HostConfig: %s, %s", n.Name, n.Spec.PodCIDR)
		return nil, err
	}
	// We need to get the IP for the podCIDR and increment it to the
	// first IP in the CIDR.
	tunIp := ip.To4()
	tunIp[3]++

	kvp := &model.KVPair{
		Key: model.HostConfigKey{
			Hostname: n.Name,
			Name:     "IpInIpTunnelAddr",
		},
		Value: tunIp.String(),
	}

	return kvp, nil
}
