/*
Copyright 2018 The Kubernetes Authors.

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

package clusterdeployer

import (
	"io/ioutil"
	"os"
	"strings"
	"time"

	"github.com/openshift/cluster-api/cmd/clusterctl/clusterdeployer/bootstrap"
	"github.com/openshift/cluster-api/cmd/clusterctl/clusterdeployer/clusterclient"
	"github.com/openshift/cluster-api/cmd/clusterctl/phases"
	clusterv1 "github.com/openshift/cluster-api/pkg/apis/cluster/v1alpha1"
	"github.com/openshift/cluster-api/pkg/util"
	"github.com/pkg/errors"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog"
)

// Deprecated interface for Provider specific logic. Please do not extend or add. This interface should be removed
// once issues/158 and issues/160 below are fixed.
type ProviderDeployer interface {
	// TODO: This requirement can be removed once after: https://github.com/kubernetes-sigs/cluster-api/issues/158
	GetIP(cluster *clusterv1.Cluster, machine *clusterv1.Machine) (string, error)
	// TODO: This requirement can be removed after: https://github.com/kubernetes-sigs/cluster-api/issues/160
	GetKubeConfig(cluster *clusterv1.Cluster, controlPlaneMachine *clusterv1.Machine) (string, error)
}

type ProviderComponentsStore interface {
	Save(providerComponents string) error
	Load() (string, error)
}

type ProviderComponentsStoreFactory interface {
	NewFromCoreClientset(clientset *kubernetes.Clientset) (ProviderComponentsStore, error)
}

type ClusterDeployer struct {
	bootstrapProvisioner    bootstrap.ClusterProvisioner
	clientFactory           clusterclient.Factory
	providerComponents      string
	addonComponents         string
	cleanupBootstrapCluster bool
}

func New(
	bootstrapProvisioner bootstrap.ClusterProvisioner,
	clientFactory clusterclient.Factory,
	providerComponents string,
	addonComponents string,
	cleanupBootstrapCluster bool) *ClusterDeployer {
	return &ClusterDeployer{
		bootstrapProvisioner:    bootstrapProvisioner,
		clientFactory:           clientFactory,
		providerComponents:      providerComponents,
		addonComponents:         addonComponents,
		cleanupBootstrapCluster: cleanupBootstrapCluster,
	}
}

const (
	retryKubeConfigReady   = 10 * time.Second
	timeoutKubeconfigReady = 20 * time.Minute
)

// Create the cluster from the provided cluster definition and machine list.
func (d *ClusterDeployer) Create(cluster *clusterv1.Cluster, machines []*clusterv1.Machine, provider ProviderDeployer, kubeconfigOutput string, providerComponentsStoreFactory ProviderComponentsStoreFactory) error {
	controlPlaneMachine, nodes, err := extractControlPlaneMachine(machines)
	if err != nil {
		return errors.Wrap(err, "unable to separate control plane machines from node machines")
	}

	bootstrapClient, cleanupBootstrapCluster, err := phases.CreateBootstrapCluster(d.bootstrapProvisioner, d.cleanupBootstrapCluster, d.clientFactory)
	defer cleanupBootstrapCluster()
	if err != nil {
		return errors.Wrap(err, "could not create bootstrap cluster")
	}
	defer closeClient(bootstrapClient, "bootstrap")

	klog.Info("Applying Cluster API stack to bootstrap cluster")
	if err := phases.ApplyClusterAPIComponents(bootstrapClient, d.providerComponents); err != nil {
		return errors.Wrap(err, "unable to apply cluster api stack to bootstrap cluster")
	}

	klog.Info("Provisioning target cluster via bootstrap cluster")
	if err := phases.ApplyCluster(bootstrapClient, cluster); err != nil {
		return errors.Wrapf(err, "unable to create cluster %q in bootstrap cluster", cluster.Name)
	}

	if cluster.Namespace == "" {
		cluster.Namespace = bootstrapClient.GetContextNamespace()
	}

	klog.Infof("Creating control plane %v in namespace %q", controlPlaneMachine.Name, cluster.Namespace)
	if err := phases.ApplyMachines(bootstrapClient, cluster.Namespace, []*clusterv1.Machine{controlPlaneMachine}); err != nil {
		return errors.Wrap(err, "unable to create control plane machine")
	}

	klog.Infof("Updating bootstrap cluster object for cluster %v in namespace %q with control plane endpoint running on %s", cluster.Name, cluster.Namespace, controlPlaneMachine.Name)
	if err := d.updateClusterEndpoint(bootstrapClient, provider, cluster.Name, cluster.Namespace); err != nil {
		return errors.Wrap(err, "unable to update bootstrap cluster endpoint")
	}

	klog.Info("Creating target cluster")
	targetClient, err := d.createTargetClusterClient(bootstrapClient, provider, kubeconfigOutput, cluster.Name, cluster.Namespace)
	if err != nil {
		return errors.Wrap(err, "unable to create target cluster")
	}
	defer closeClient(targetClient, "target")

	if d.addonComponents != "" {
		if err := phases.ApplyAddons(targetClient, d.addonComponents); err != nil {
			return errors.Wrap(err, "unable to apply addons to target cluster")
		}
	}

	klog.Info("Applying Cluster API stack to target cluster")
	if err := d.applyClusterAPIComponentsWithPivoting(targetClient, bootstrapClient, cluster.Namespace); err != nil {
		return errors.Wrap(err, "unable to apply cluster api stack to target cluster")
	}

	klog.Info("Saving provider components to the target cluster")
	err = d.saveProviderComponentsToCluster(providerComponentsStoreFactory, kubeconfigOutput)
	if err != nil {
		return errors.Wrap(err, "unable to save provider components to target cluster")
	}

	err = targetClient.EnsureNamespace(cluster.Namespace)
	if err != nil {
		return errors.Wrapf(err, "unable to ensure namespace %q in targetCluster", cluster.Namespace)
	}

	// For some reason, endpoint doesn't get updated in bootstrap cluster sometimes. So we
	// update the target cluster endpoint as well to be sure.
	klog.Infof("Updating target cluster object with control plane endpoint running on %s", controlPlaneMachine.Name)
	if err := d.updateClusterEndpoint(targetClient, provider, cluster.Name, cluster.Namespace); err != nil {
		return errors.Wrap(err, "unable to update target cluster endpoint")
	}

	klog.Info("Creating node machines in target cluster.")
	if err := phases.ApplyMachines(targetClient, cluster.Namespace, nodes); err != nil {
		return errors.Wrap(err, "unable to create node machines")
	}

	klog.Infof("Done provisioning cluster. You can now access your cluster with kubectl --kubeconfig %v", kubeconfigOutput)

	return nil
}

func (d *ClusterDeployer) Delete(targetClient clusterclient.Client, namespace string) error {
	klog.Info("Creating bootstrap cluster")
	bootstrapClient, cleanupBootstrapCluster, err := phases.CreateBootstrapCluster(d.bootstrapProvisioner, d.cleanupBootstrapCluster, d.clientFactory)
	defer cleanupBootstrapCluster()
	if err != nil {
		return errors.Wrap(err, "could not create bootstrap cluster")
	}
	defer closeClient(bootstrapClient, "bootstrap")

	klog.Info("Applying Cluster API stack to bootstrap cluster")
	if err := phases.ApplyClusterAPIComponents(bootstrapClient, d.providerComponents); err != nil {
		return errors.Wrap(err, "unable to apply cluster api stack to bootstrap cluster")
	}

	klog.Info("Deleting Cluster API Provider Components from target cluster")
	if err = targetClient.Delete(d.providerComponents); err != nil {
		klog.Infof("error while removing provider components from target cluster: %v", err)
		klog.Infof("Continuing with a best effort delete")
	}

	klog.Info("Copying objects from target cluster to bootstrap cluster")
	if err = pivotNamespace(targetClient, bootstrapClient, namespace); err != nil {
		return errors.Wrap(err, "unable to copy objects from target to bootstrap cluster")
	}

	klog.Info("Deleting objects from bootstrap cluster")
	if err = deleteObjectsInNamespace(bootstrapClient, namespace); err != nil {
		return errors.Wrap(err, "unable to finish deleting objects in bootstrap cluster, resources may have been leaked")
	}

	klog.Info("Deletion of cluster complete")

	return nil
}

func (d *ClusterDeployer) createTargetClusterClient(bootstrapClient clusterclient.Client, provider ProviderDeployer, kubeconfigOutput string, clusterName, namespace string) (clusterclient.Client, error) {
	cluster, controlPlane, _, err := getClusterAPIObject(bootstrapClient, clusterName, namespace)
	if err != nil {
		return nil, err
	}

	klog.V(1).Info("Getting target cluster kubeconfig.")
	targetKubeconfig, err := waitForKubeconfigReady(provider, cluster, controlPlane)
	if err != nil {
		return nil, errors.Wrap(err, "unable to get target cluster kubeconfig")
	}

	if err = d.writeKubeconfig(targetKubeconfig, kubeconfigOutput); err != nil {
		return nil, err
	}

	targetClient, err := d.clientFactory.NewClientFromKubeconfig(targetKubeconfig)
	if err != nil {
		return nil, errors.Wrap(err, "unable to create target cluster client")
	}

	return targetClient, nil
}

func (d *ClusterDeployer) updateClusterEndpoint(client clusterclient.Client, provider ProviderDeployer, clusterName, namespace string) error {
	// Update cluster endpoint. Needed till this logic moves into cluster controller.
	// TODO: https://github.com/kubernetes-sigs/cluster-api/issues/158
	// Fetch fresh objects.
	cluster, controlPlane, _, err := getClusterAPIObject(client, clusterName, namespace)
	if err != nil {
		return err
	}
	clusterEndpoint, err := provider.GetIP(cluster, controlPlane)
	if err != nil {
		return errors.Wrap(err, "unable to get cluster endpoint")
	}
	err = client.UpdateClusterObjectEndpoint(clusterEndpoint, clusterName, namespace)
	if err != nil {
		return errors.Wrap(err, "unable to update cluster endpoint")
	}
	return nil
}

func (d *ClusterDeployer) saveProviderComponentsToCluster(factory ProviderComponentsStoreFactory, kubeconfigPath string) error {
	clientset, err := d.clientFactory.NewCoreClientsetFromKubeconfigFile(kubeconfigPath)
	if err != nil {
		return errors.Wrap(err, "error creating core clientset")
	}
	pcStore, err := factory.NewFromCoreClientset(clientset)
	if err != nil {
		return errors.Wrap(err, "unable to create provider components store")
	}
	err = pcStore.Save(d.providerComponents)
	if err != nil {
		return errors.Wrap(err, "error saving provider components")
	}
	return nil
}

func (d *ClusterDeployer) applyClusterAPIComponentsWithPivoting(client, source clusterclient.Client, namespace string) error {
	klog.Info("Applying Cluster API Provider Components")
	if err := client.Apply(d.providerComponents); err != nil {
		return errors.Wrap(err, "unable to apply cluster api controllers")
	}

	klog.Info("Pivoting Cluster API objects from bootstrap to target cluster.")
	err := pivotNamespace(source, client, namespace)
	if err != nil {
		return errors.Wrap(err, "unable to pivot cluster API objects")
	}

	return nil
}

func (d *ClusterDeployer) writeKubeconfig(kubeconfig string, kubeconfigOutput string) error {
	const fileMode = 0666
	os.Remove(kubeconfigOutput)
	return ioutil.WriteFile(kubeconfigOutput, []byte(kubeconfig), fileMode)
}

func waitForKubeconfigReady(provider ProviderDeployer, cluster *clusterv1.Cluster, machine *clusterv1.Machine) (string, error) {
	kubeconfig := ""
	err := util.PollImmediate(retryKubeConfigReady, timeoutKubeconfigReady, func() (bool, error) {
		klog.V(2).Infof("Waiting for kubeconfig on %v to become ready...", machine.Name)
		k, err := provider.GetKubeConfig(cluster, machine)
		if err != nil {
			klog.V(4).Infof("error getting kubeconfig: %v", err)
			return false, nil
		}
		if k == "" {
			return false, nil
		}
		kubeconfig = k
		return true, nil
	})

	return kubeconfig, err
}

func pivotNamespace(from, to clusterclient.Client, namespace string) error {
	if err := from.WaitForClusterV1alpha1Ready(); err != nil {
		return errors.New("cluster v1alpha1 resource not ready on source cluster")
	}

	if err := to.WaitForClusterV1alpha1Ready(); err != nil {
		return errors.New("cluster v1alpha1 resource not ready on target cluster")
	}

	clusters, err := from.GetClusterObjectsInNamespace(namespace)
	if err != nil {
		return err
	}

	for _, cluster := range clusters {
		// New objects cannot have a specified resource version. Clear it out.
		cluster.SetResourceVersion("")
		if err = to.CreateClusterObject(cluster); err != nil {
			return errors.Wrapf(err, "error moving Cluster %q", cluster.GetName())
		}
		klog.Infof("Moved Cluster '%s'", cluster.GetName())
	}

	fromDeployments, err := from.GetMachineDeploymentObjectsInNamespace(namespace)
	if err != nil {
		return err
	}
	for _, deployment := range fromDeployments {
		// New objects cannot have a specified resource version. Clear it out.
		deployment.SetResourceVersion("")
		if err = to.CreateMachineDeploymentObjects([]*clusterv1.MachineDeployment{deployment}, namespace); err != nil {
			return errors.Wrapf(err, "error moving MachineDeployment %q", deployment.GetName())
		}
		klog.Infof("Moved MachineDeployment %v", deployment.GetName())
	}

	fromMachineSets, err := from.GetMachineSetObjectsInNamespace(namespace)
	if err != nil {
		return err
	}
	for _, machineSet := range fromMachineSets {
		// New objects cannot have a specified resource version. Clear it out.
		machineSet.SetResourceVersion("")
		if err := to.CreateMachineSetObjects([]*clusterv1.MachineSet{machineSet}, namespace); err != nil {
			return errors.Wrapf(err, "error moving MachineSet %q", machineSet.GetName())
		}
		klog.Infof("Moved MachineSet %v", machineSet.GetName())
	}

	machines, err := from.GetMachineObjectsInNamespace(namespace)
	if err != nil {
		return err
	}

	for _, machine := range machines {
		// New objects cannot have a specified resource version. Clear it out.
		machine.SetResourceVersion("")
		if err = to.CreateMachineObjects([]*clusterv1.Machine{machine}, namespace); err != nil {
			return errors.Wrapf(err, "error moving Machine %q", machine.GetName())
		}
		klog.Infof("Moved Machine '%s'", machine.GetName())
	}
	return nil
}

func deleteObjectsInNamespace(client clusterclient.Client, namespace string) error {
	var errorList []string
	klog.Infof("Deleting machine deployments in namespace %q", namespace)
	if err := client.DeleteMachineDeploymentObjectsInNamespace(namespace); err != nil {
		err = errors.Wrap(err, "error deleting machine deployments")
		errorList = append(errorList, err.Error())
	}
	klog.Infof("Deleting machine sets in namespace %q", namespace)
	if err := client.DeleteMachineSetObjectsInNamespace(namespace); err != nil {
		err = errors.Wrap(err, "error deleting machine sets")
		errorList = append(errorList, err.Error())
	}
	klog.Infof("Deleting machines in namespace %q", namespace)
	if err := client.DeleteMachineObjectsInNamespace(namespace); err != nil {
		err = errors.Wrap(err, "error deleting machines")
		errorList = append(errorList, err.Error())
	}
	klog.Infof("Deleting clusters in namespace %q", namespace)
	if err := client.DeleteClusterObjectsInNamespace(namespace); err != nil {
		err = errors.Wrap(err, "error deleting clusters")
		errorList = append(errorList, err.Error())
	}
	klog.Infof("Deleting namespace %q", namespace)
	if err := client.DeleteNamespace(namespace); err != nil {
		err = errors.Wrap(err, "error deleting namespace")
		errorList = append(errorList, err.Error())
	}
	if len(errorList) > 0 {
		return errors.Errorf("error(s) encountered deleting objects from bootstrap cluster: [%v]", strings.Join(errorList, ", "))
	}
	return nil
}

func getClusterAPIObject(client clusterclient.Client, clusterName, namespace string) (*clusterv1.Cluster, *clusterv1.Machine, []*clusterv1.Machine, error) {
	machines, err := client.GetMachineObjectsInNamespace(namespace)
	if err != nil {
		return nil, nil, nil, errors.Wrap(err, "unable to fetch machines")
	}
	cluster, err := client.GetClusterObject(clusterName, namespace)
	if err != nil {
		return nil, nil, nil, errors.Wrapf(err, "unable to fetch cluster %v in namespace %v", clusterName, namespace)
	}

	controlPlaneMachine, nodes, err := extractControlPlaneMachine(machines)
	if err != nil {
		return nil, nil, nil, errors.Wrapf(err, "unable to fetch control plane machine in cluster %v in namespace %v", clusterName, namespace)
	}
	return cluster, controlPlaneMachine, nodes, nil
}

// extractControlPlaneMachine separates the machines running the control plane (singular) from the incoming machines.
// This is currently done by looking at which machine specifies the control plane version.
// TODO: Cleanup.
func extractControlPlaneMachine(machines []*clusterv1.Machine) (*clusterv1.Machine, []*clusterv1.Machine, error) {
	nodes := []*clusterv1.Machine{}
	controlPlaneMachines := []*clusterv1.Machine{}
	for _, machine := range machines {
		if util.IsControlPlaneMachine(machine) {
			controlPlaneMachines = append(controlPlaneMachines, machine)
		} else {
			nodes = append(nodes, machine)
		}
	}
	if len(controlPlaneMachines) != 1 {
		return nil, nil, errors.Errorf("expected one control plane machine, got: %v", len(controlPlaneMachines))
	}
	return controlPlaneMachines[0], nodes, nil
}

func closeClient(client clusterclient.Client, name string) {
	if err := client.Close(); err != nil {
		klog.Errorf("Could not close %v client: %v", name, err)
	}
}
