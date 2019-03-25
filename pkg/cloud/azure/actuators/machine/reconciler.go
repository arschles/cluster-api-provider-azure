/*
Copyright 2019 The Kubernetes Authors.

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

package machine

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2018-10-01/compute"
	"github.com/pkg/errors"
	apicorev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog"
	"sigs.k8s.io/cluster-api-provider-azure/pkg/apis/azureprovider/v1alpha1"
	"sigs.k8s.io/cluster-api-provider-azure/pkg/cloud/azure"
	"sigs.k8s.io/cluster-api-provider-azure/pkg/cloud/azure/actuators"
	"sigs.k8s.io/cluster-api-provider-azure/pkg/cloud/azure/converters"
	"sigs.k8s.io/cluster-api-provider-azure/pkg/cloud/azure/services/certificates"
	"sigs.k8s.io/cluster-api-provider-azure/pkg/cloud/azure/services/config"
	"sigs.k8s.io/cluster-api-provider-azure/pkg/cloud/azure/services/networkinterfaces"
	"sigs.k8s.io/cluster-api-provider-azure/pkg/cloud/azure/services/virtualmachineextensions"
	"sigs.k8s.io/cluster-api-provider-azure/pkg/cloud/azure/services/virtualmachines"
	clusterutil "sigs.k8s.io/cluster-api/pkg/util"
)

const (
	// DefaultBootstrapTokenTTL default ttl for bootstrap token
	DefaultBootstrapTokenTTL = 10 * time.Minute
)

// Reconciler are list of services required by cluster actuator, easy to create a fake
type Reconciler struct {
	scope                 *actuators.MachineScope
	networkInterfacesSvc  azure.Service
	virtualMachinesSvc    azure.Service
	virtualMachinesExtSvc azure.Service
}

// NewReconciler populates all the services based on input scope
func NewReconciler(scope *actuators.MachineScope) *Reconciler {
	return &Reconciler{
		scope:                 scope,
		networkInterfacesSvc:  networkinterfaces.NewService(scope.Scope),
		virtualMachinesSvc:    virtualmachines.NewService(scope.Scope),
		virtualMachinesExtSvc: virtualmachineextensions.NewService(scope.Scope),
	}
}

// Create creates machine if and only if machine exists, handled by cluster-api
func (s *Reconciler) Create(ctx context.Context) error {
	bootstrapToken, err := s.checkControlPlaneMachines()
	if err != nil {
		return errors.Wrap(err, "failed to check control plane machines in cluster")
	}

	networkInterfaceSpec := &networkinterfaces.Spec{
		Name:     fmt.Sprintf("%s-nic", s.scope.Machine.Name),
		VnetName: azure.GenerateVnetName(s.scope.Cluster.Name),
	}
	switch set := s.scope.Machine.ObjectMeta.Labels["set"]; set {
	case v1alpha1.Node:
		networkInterfaceSpec.SubnetName = azure.GenerateNodeSubnetName(s.scope.Cluster.Name)
	case v1alpha1.ControlPlane:
		networkInterfaceSpec.SubnetName = azure.GenerateControlPlaneSubnetName(s.scope.Cluster.Name)
		networkInterfaceSpec.PublicLoadBalancerName = azure.GeneratePublicLBName(s.scope.Cluster.Name)
		networkInterfaceSpec.InternalLoadBalancerName = azure.GenerateInternalLBName(s.scope.Cluster.Name)
		networkInterfaceSpec.NatRule = 0
	default:
		return errors.Errorf("Unknown value %s for label `set` on machine %s, skipping machine creation", set, s.scope.Machine.Name)
	}

	err = s.networkInterfacesSvc.CreateOrUpdate(ctx, networkInterfaceSpec)
	if err != nil {
		return errors.Wrap(err, "Unable to create VM network interface")
	}

	decoded, err := base64.StdEncoding.DecodeString(s.scope.MachineConfig.SSHPublicKey)
	if err != nil {
		errors.Wrapf(err, "failed to decode ssh public key")
	}

	vmSpec := &virtualmachines.Spec{
		Name:       s.scope.Machine.Name,
		NICName:    networkInterfaceSpec.Name,
		SSHKeyData: string(decoded),
		Size:       s.scope.MachineConfig.VMSize,
		OSDisk:     s.scope.MachineConfig.OSDisk,
		Image:      s.scope.MachineConfig.Image,
	}
	err = s.virtualMachinesSvc.CreateOrUpdate(ctx, vmSpec)
	if err != nil {
		return errors.Wrapf(err, "failed to create or get machine")
	}

	scriptData, err := config.GetVMStartupScript(s.scope, bootstrapToken)
	if err != nil {
		return errors.Wrapf(err, "failed to get vm startup script")
	}

	vmExtSpec := &virtualmachineextensions.Spec{
		Name:       "startupScript",
		VMName:     s.scope.Machine.Name,
		ScriptData: base64.StdEncoding.EncodeToString([]byte(scriptData)),
	}
	err = s.virtualMachinesExtSvc.CreateOrUpdate(ctx, vmExtSpec)
	if err != nil {

	}

	// TODO: update once machine controllers have a way to indicate a machine has been provisoned. https://github.com/kubernetes-sigs/cluster-api/issues/253
	// Seeing a node cannot be purely relied upon because the provisioned control plane will not be registering with
	// the stack that provisions it.
	if s.scope.Machine.Annotations == nil {
		s.scope.Machine.Annotations = map[string]string{}
	}

	s.scope.Machine.Annotations["cluster-api-provider-azure"] = "true"

	return nil
}

// Update updates machine if and only if machine exists, handled by cluster-api
func (s *Reconciler) Update(ctx context.Context) error {
	vmSpec := &virtualmachines.Spec{
		Name: s.scope.Machine.Name,
	}
	vmInterface, err := s.virtualMachinesSvc.Get(ctx, vmSpec)
	if err != nil {
		return errors.Errorf("failed to get vm: %+v", err)
	}

	vm, ok := vmInterface.(compute.VirtualMachine)
	if !ok {
		return errors.New("returned incorrect vm interface")
	}

	// We can now compare the various Azure state to the state we were passed.
	// We will check immutable state first, in order to fail quickly before
	// moving on to state that we can mutate.
	if isMachineOutdated(s.scope.MachineConfig, converters.SDKToVM(vm)) {
		return errors.Errorf("found attempt to change immutable state")
	}

	// TODO: Uncomment after implementing tagging.
	// Ensure that the tags are correct.
	/*
		_, err = a.ensureTags(computeSvc, machine, scope.MachineStatus.VMID, scope.MachineConfig.AdditionalTags)
		if err != nil {
			return errors.Errorf("failed to ensure tags: %+v", err)
		}
	*/

	return nil
}

// Exists checks if machine exists
func (s *Reconciler) Exists(ctx context.Context) (bool, error) {
	exists, err := s.isVMExists(ctx)
	if err != nil {
		return false, err
	} else if !exists {
		return false, nil
	}

	switch *s.scope.MachineStatus.VMState {
	case v1alpha1.VMStateSucceeded:
		klog.Infof("Machine %v is running", *s.scope.MachineStatus.VMID)
	case v1alpha1.VMStateUpdating:
		klog.Infof("Machine %v is updating", *s.scope.MachineStatus.VMID)
	default:
		return false, nil
	}

	if s.scope.Machine.Spec.ProviderID == nil || *s.scope.Machine.Spec.ProviderID == "" {
		// TODO: This should be unified with the logic for getting the nodeRef, and
		// should potentially leverage the code that already exists in
		// kubernetes/cloud-provider-azure
		providerID := fmt.Sprintf("azure:////%s", *s.scope.MachineStatus.VMID)
		s.scope.Machine.Spec.ProviderID = &providerID
	}

	// Set the Machine NodeRef.
	if s.scope.Machine.Status.NodeRef == nil {
		nodeRef, err := getNodeReference(s.scope)
		if err != nil {
			klog.Warningf("Failed to set nodeRef: %v", err)
			return true, nil
		}

		s.scope.Machine.Status.NodeRef = nodeRef
		klog.Infof("Setting machine %s nodeRef to %s", s.scope.Name(), nodeRef.Name)
	}

	return true, nil
}

// Delete reconciles all the services in pre determined order
func (s *Reconciler) Delete(ctx context.Context) error {
	vmSpec := &virtualmachines.Spec{
		Name: s.scope.Machine.Name,
	}

	err := s.virtualMachinesSvc.Delete(ctx, vmSpec)
	if err != nil {
		return errors.Wrapf(err, "failed to delete machine")
	}

	networkInterfaceSpec := &networkinterfaces.Spec{
		Name:     fmt.Sprintf("%s-nic", s.scope.Machine.Name),
		VnetName: azure.GenerateVnetName(s.scope.Cluster.Name),
	}

	err = s.networkInterfacesSvc.Delete(ctx, networkInterfaceSpec)
	if err != nil {
		return errors.Wrapf(err, "Unable to delete network interface")
	}

	return nil
}

// isMachineOutdated checks that no immutable fields have been updated in an
// Update request.
// Returns a bool indicating if an attempt to change immutable state occurred.
//  - true:  An attempt to change immutable state occurred.
//  - false: Immutable state was untouched.
func isMachineOutdated(machineSpec *v1alpha1.AzureMachineProviderSpec, vm *v1alpha1.VM) bool {
	// VM Size
	if machineSpec.VMSize != vm.VMSize {
		return true
	}

	// TODO: Add additional checks for immutable fields

	// No immutable state changes found.
	return false
}

func (s *Reconciler) isNodeJoin() (bool, error) {
	clusterMachines, err := s.scope.MachineClient.List(metav1.ListOptions{})
	if err != nil {
		return false, errors.Wrapf(err, "failed to retrieve machines in cluster")
	}

	switch set := s.scope.Machine.ObjectMeta.Labels["set"]; set {
	case v1alpha1.Node:
		return true, nil
	case v1alpha1.ControlPlane:
		for _, cm := range clusterMachines.Items {
			if !clusterutil.IsControlPlaneMachine(&cm) {
				continue
			}
			vmInterface, err := s.virtualMachinesSvc.Get(context.Background(), &virtualmachines.Spec{Name: cm.Name})
			if err != nil && vmInterface == nil {
				klog.V(2).Infof("Machine %s should join the controlplane: false", s.scope.Name())
				return false, nil
			}

			if err != nil {
				return false, errors.Wrapf(err, "failed to verify existence of machine %s", cm.Name)
			}

			vmExtSpec := &virtualmachineextensions.Spec{
				Name:   "startupScript",
				VMName: cm.Name,
			}

			vmExt, err := s.virtualMachinesExtSvc.Get(context.Background(), vmExtSpec)
			if err != nil && vmExt == nil {
				klog.V(2).Infof("Machine %s should join the controlplane: false", cm.Name)
				return false, nil
			}

			klog.V(2).Infof("Machine %s should join the controlplane: true", s.scope.Name())
			return true, nil
		}

		return false, nil
	default:
		return false, errors.Errorf("Unknown value %s for label `set` on machine %s, skipping machine creation", set, s.scope.Name())
	}
}

func (s *Reconciler) checkControlPlaneMachines() (string, error) {
	isJoin, err := s.isNodeJoin()
	if err != nil {
		return "", errors.Wrapf(err, "failed to determine whether machine should join cluster")
	}

	var bootstrapToken string
	if isJoin {
		if s.scope.ClusterConfig == nil {
			return "", errors.Errorf("failed to retrieve corev1 client for empty kubeconfig")
		}
		bootstrapToken, err = certificates.CreateNewBootstrapToken(s.scope.ClusterConfig.AdminKubeconfig, DefaultBootstrapTokenTTL)
		if err != nil {
			return "", errors.Wrapf(err, "failed to create new bootstrap token")
		}
	}
	return bootstrapToken, nil
}

func coreV1Client(kubeconfig string) (corev1.CoreV1Interface, error) {
	clientConfig, err := clientcmd.NewClientConfigFromBytes([]byte(kubeconfig))

	if err != nil {
		return nil, errors.Wrapf(err, "failed to get client config for cluster")
	}

	cfg, err := clientConfig.ClientConfig()
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get client config for cluster")
	}

	return corev1.NewForConfig(cfg)
}

func (s *Reconciler) isVMExists(ctx context.Context) (bool, error) {
	vmSpec := &virtualmachines.Spec{
		Name: s.scope.Name(),
	}
	vmInterface, err := s.virtualMachinesSvc.Get(ctx, vmSpec)

	if err != nil && vmInterface == nil {
		return false, nil
	}

	if err != nil {
		return false, errors.Wrap(err, "Failed to get vm")
	}

	vm, ok := vmInterface.(compute.VirtualMachine)
	if !ok {
		return false, errors.New("returned incorrect vm interface")
	}

	klog.Infof("Found vm for machine %s", s.scope.Name())

	vmExtSpec := &virtualmachineextensions.Spec{
		Name:   "startupScript",
		VMName: s.scope.Name(),
	}

	vmExt, err := s.virtualMachinesExtSvc.Get(ctx, vmExtSpec)
	if err != nil && vmExt == nil {
		return false, nil
	}

	if err != nil {
		return false, errors.Wrapf(err, "failed to get vm extension")
	}

	vmState := v1alpha1.VMState(*vm.ProvisioningState)

	s.scope.MachineStatus.VMID = vm.ID
	s.scope.MachineStatus.VMState = &vmState
	return true, nil
}

func getNodeReference(scope *actuators.MachineScope) (*apicorev1.ObjectReference, error) {
	if scope.MachineStatus.VMID == nil {
		return nil, errors.Errorf("instance id is empty for machine %s", scope.Machine.Name)
	}

	instanceID := *scope.MachineStatus.VMID

	if scope.ClusterConfig == nil {
		return nil, errors.Errorf("failed to retrieve corev1 client for empty kubeconfig %s", scope.Cluster.Name)
	}

	coreClient, err := coreV1Client(scope.ClusterConfig.AdminKubeconfig)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to retrieve corev1 client for cluster %s", scope.Cluster.Name)
	}

	listOpt := metav1.ListOptions{}

	for {
		nodeList, err := coreClient.Nodes().List(listOpt)
		if err != nil {
			return nil, errors.Wrap(err, "failed to query cluster nodes")
		}

		for _, node := range nodeList.Items {
			// TODO(vincepri): Improve this comparison without relying on substrings.
			if strings.Contains(node.Spec.ProviderID, instanceID) {
				return &apicorev1.ObjectReference{
					Kind:       node.Kind,
					APIVersion: node.APIVersion,
					Name:       node.Name,
				}, nil
			}
		}

		listOpt.Continue = nodeList.Continue
		if listOpt.Continue == "" {
			break
		}
	}

	return nil, errors.Errorf("no node found for machine %s", scope.Name())
}