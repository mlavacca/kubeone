/*
Copyright 2021 The KubeOne Authors.

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

package tasks

import (
	"fmt"
	"strconv"
	"time"

	"github.com/pkg/errors"

	kubeoneapi "k8c.io/kubeone/pkg/apis/kubeone"
	"k8c.io/kubeone/pkg/nodeutils"
	"k8c.io/kubeone/pkg/scripts"
	"k8c.io/kubeone/pkg/ssh"
	"k8c.io/kubeone/pkg/state"

	"github.com/kubermatic/machine-controller/pkg/apis/cluster/common"
	clusterv1alpha1 "github.com/kubermatic/machine-controller/pkg/apis/cluster/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	provisionedByAnnotation            = "pv.kubernetes.io/provisioned-by"
	provisionedByOpenStackInTreeCinder = "kubernetes.io/cinder"
	provisionedByOpenStackCSICinder    = "cinder.csi.openstack.org"
)

func validateExternalCloudProviderConfig(s *state.State) error {
	if !s.Cluster.CloudProvider.External {
		return errors.New(".cloudProvider.external must be enabled to start the migration")
	}
	if !s.Cluster.CloudProvider.CSIMigrationSupported() {
		return errors.New("ccm/csi migration is not supported for the specified provider")
	}
	if !s.LiveCluster.CCMStatus.InTreeCloudProviderEnabled {
		return errors.New("the cluster is already running external ccm")
	} else if s.LiveCluster.CCMStatus.ExternalCCMDeployed && !s.CCMMigrationComplete {
		return errors.New("the ccm/csi migration is currently in progress, run command with --complete to finish it")
	}
	if s.Cluster.CloudProvider.Vsphere != nil && s.Cluster.CloudProvider.CSIConfig == "" {
		return errors.New("the ccm/csi migration for vsphere requires providing csi configuration using .cloudProvider.csiConfig field")
	}
	if len(s.Cluster.StaticWorkers.Hosts) > 0 {
		return errors.New("the ccm/csi migration for cluster with static worker nodes is currently unsupported")
	}

	return nil
}

func readyToCompleteMigration(s *state.State) error {
	if s.DynamicClient == nil {
		return errors.New("clientset not initialized")
	}

	machines := clusterv1alpha1.MachineList{}
	if err := s.DynamicClient.List(s.Context, &machines); err != nil {
		return errors.Wrap(err, "failed to list machines")
	}

	migrated := true
	for i := range machines.Items {
		flag := common.GetKubeletFlags(&machines.Items[i])[common.ExternalCloudProviderKubeletFlag]
		if boolFlag, err := strconv.ParseBool(flag); !boolFlag || err != nil {
			migrated = false
			break
		}
	}

	if !migrated {
		return errors.New("not all machines are rolled-out or migration not started yet")
	}

	return nil
}

func regenerateControlPlaneManifests(s *state.State) error {
	return s.RunTaskOnControlPlane(regenerateControlPlaneManifestsInternal, state.RunSequentially)
}

func regenerateControlPlaneManifestsInternal(s *state.State, node *kubeoneapi.HostConfig, conn ssh.Connection) error {
	logger := s.Logger.WithField("node", node.PublicAddress)
	logger.Info("Regenerating Kubernetes API server and kube-controller-manager manifests...")

	var (
		apiserverPodName         = fmt.Sprintf("kube-apiserver-%s", node.Hostname)
		controllerManagerPodName = fmt.Sprintf("kube-controller-manager-%s", node.Hostname)
	)

	cmd, err := scripts.CCMMigrationRegenerateControlPlaneManifests(s.WorkDir, node.ID, s.KubeadmVerboseFlag())
	if err != nil {
		return err
	}
	_, _, err = s.Runner.RunRaw(cmd)
	if err != nil {
		return err
	}

	timeout := 30 * time.Second
	logger.Infof("Waiting %s for Kubelet to roll-out static pods...", timeout)
	time.Sleep(timeout)

	timeout = 2 * time.Minute
	logger.Infof("Waiting up to %s for API server to become healthy...", timeout)
	err = waitForStaticPodReady(s, timeout, apiserverPodName, metav1.NamespaceSystem)
	if err != nil {
		return errors.Wrapf(err, "API server failed to come up for %s", timeout)
	}

	logger.Infof("Waiting up to %s for kube-controller-manager roll-out...", timeout)
	err = waitForStaticPodReady(s, timeout, controllerManagerPodName, metav1.NamespaceSystem)
	if err != nil {
		return errors.Wrapf(err, "API server failed to come up for %s", timeout)
	}

	return nil
}

func updateKubeletConfig(s *state.State) error {
	return s.RunTaskOnControlPlane(updateKubeletConfigInternal, state.RunSequentially)
}

func updateKubeletConfigInternal(s *state.State, node *kubeoneapi.HostConfig, conn ssh.Connection) error {
	logger := s.Logger.WithField("node", node.PublicAddress)
	logger.Info("Updating config and restarting Kubelet...")

	drainer := nodeutils.NewDrainer(s.RESTConfig, logger)

	logger.Infoln("Cordoning node...")
	if err := drainer.Cordon(s.Context, node.Hostname, true); err != nil {
		return errors.Wrap(err, "failed to cordon follower control plane node")
	}

	logger.Infoln("Draining node...")
	if err := drainer.Drain(s.Context, node.Hostname); err != nil {
		return errors.Wrap(err, "failed to drain follower control plane node")
	}

	cmd, err := scripts.CCMMigrationUpdateKubeletConfig(s.WorkDir, node.ID, s.KubeadmVerboseFlag())
	if err != nil {
		return err
	}
	_, _, err = s.Runner.RunRaw(cmd)
	if err != nil {
		return err
	}

	timeout := 2 * time.Minute
	logger.Debugf("Waiting up to %s for Kubelet to become running...", timeout)
	err = wait.PollImmediate(5*time.Second, 2*time.Minute, func() (bool, error) {
		kubeletStatus, sErr := systemdStatus(conn, "kubelet")
		if sErr != nil {
			return false, sErr
		}

		if kubeletStatus&state.SystemDStatusRunning != 0 && kubeletStatus&state.SystemDStatusRestarting == 0 {
			return true, nil
		}

		return false, nil
	})
	if err != nil {
		return err
	}

	logger.Infoln("Uncordoning node...")
	if err := drainer.Cordon(s.Context, node.Hostname, false); err != nil {
		return errors.Wrap(err, "failed to uncordon follower control plane node")
	}

	return nil
}

func waitForStaticPodReady(s *state.State, timeout time.Duration, staticPodName, staticPodNamespace string) error {
	if s.DynamicClient == nil {
		return errors.New("clientset not initialized")
	}
	if staticPodName == "" || staticPodNamespace == "" {
		return errors.New("static pod name and namespace are required")
	}

	return wait.PollImmediate(5*time.Second, timeout, func() (bool, error) {
		if s.Verbose {
			s.Logger.Debugf("Waiting for pod %q to become healthy...", staticPodName)
		}

		pod := corev1.Pod{}
		key := client.ObjectKey{
			Name:      staticPodName,
			Namespace: staticPodNamespace,
		}
		err := s.DynamicClient.Get(s.Context, key, &pod)
		if err != nil {
			// NB: We're intentionally ignoring error here to prevent failures while
			// Kubelet is rolling-out the static pod.
			if s.Verbose {
				s.Logger.Debugf("Failed to get pod %q: %v", staticPodName, err)
			}
			return false, nil
		}

		// Ensure pod is running
		if pod.Status.Phase != corev1.PodRunning {
			if s.Verbose {
				s.Logger.Debugf("Pod %q is not yet running", staticPodName)
			}
			return false, nil
		}

		// Ensure pod and all containers are ready
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status != corev1.ConditionTrue {
				if s.Verbose {
					s.Logger.Debugf("Pod %q is not yet ready", staticPodName)
				}
				return false, nil
			} else if cond.Type == corev1.ContainersReady && cond.Status != corev1.ConditionTrue {
				if s.Verbose {
					s.Logger.Debugf("Containers for pod %q are not yet ready", staticPodName)
				}
				return false, nil
			}
		}

		return true, nil
	})
}

func migrateOpenStackPVs(s *state.State) error {
	if s.DynamicClient == nil {
		return errors.New("dynamic client is not initialized")
	}

	s.Logger.Infof("Patching OpenStack PersistentVolumes with annotation \"%s=%s\"...", provisionedByAnnotation, provisionedByOpenStackCSICinder)

	pvList := corev1.PersistentVolumeList{}
	if err := s.DynamicClient.List(s.Context, &pvList, &client.ListOptions{}); err != nil {
		return errors.Wrap(err, "failed to list persistentvolumes")
	}

	for i, pv := range pvList.Items {
		if pv.Annotations[provisionedByAnnotation] == provisionedByOpenStackInTreeCinder {
			if s.Verbose {
				s.Logger.Debugf("Patching PersistentVolume \"%s/%s\"...", pv.Namespace, pv.Name)
			}

			oldPv := pv.DeepCopy()
			pv.Annotations[provisionedByAnnotation] = provisionedByOpenStackCSICinder

			if err := s.DynamicClient.Patch(s.Context, &pvList.Items[i], client.MergeFrom(oldPv)); err != nil {
				return errors.Wrapf(err, "failed to patch persistnetvolume %q with annotation \"%s=%s\"", pv.Name, provisionedByAnnotation, provisionedByOpenStackCSICinder)
			}
		}
	}

	return nil
}
