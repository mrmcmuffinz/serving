/*
Copyright 2018 Google LLC

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

package revision

import (
	"net"
	"strings"

	"go.uber.org/zap"

	"github.com/knative/serving/pkg/apis/serving/v1alpha1"
	"github.com/knative/serving/pkg/controller"
	"github.com/knative/serving/pkg/queue"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	// Each Knative Serving pod gets 1 cpu.
	userContainerCPU    = "400m"
	queueContainerCPU   = "25m"
	fluentdContainerCPU = "75m"

	fluentdConfigMapVolumeName     = "configmap"
	varLogVolumeName               = "varlog"
	istioOutboundIPRangeAnnotation = "traffic.sidecar.istio.io/includeOutboundIPRanges"
)

func hasHTTPPath(p *corev1.Probe) bool {
	if p == nil {
		return false
	}
	if p.Handler.HTTPGet == nil {
		return false
	}
	return p.Handler.HTTPGet.Path != ""
}

// MakeElaPodSpec creates a pod spec.
func MakeElaPodSpec(
	rev *v1alpha1.Revision,
	controllerConfig *ControllerConfig) *corev1.PodSpec {
	varLogVolume := corev1.Volume{
		Name: varLogVolumeName,
		VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		},
	}

	userContainer := rev.Spec.Container.DeepCopy()
	// Adding or removing an overwritten corev1.Container field here? Don't forget to
	// update the validations in pkg/webhook.validateContainer.
	userContainer.Name = userContainerName
	userContainer.Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceName("cpu"): resource.MustParse(userContainerCPU),
		},
	}
	userContainer.Ports = []corev1.ContainerPort{{
		Name:          userPortName,
		ContainerPort: int32(userPort),
	}}
	userContainer.VolumeMounts = append(
		userContainer.VolumeMounts,
		corev1.VolumeMount{
			Name:      varLogVolumeName,
			MountPath: "/var/log",
		},
	)
	// Add our own PreStop hook here, which should do two things:
	// - make the container fails the next readinessCheck to avoid
	//   having more traffic, and
	// - add a small delay so that the container stays alive a little
	//   bit longer in case stoppage of traffic is not effective
	//   immediately.
	//
	// TODO(tcnghia): Fail validation webhook when users specify their
	// own lifecycle hook.
	userContainer.Lifecycle = &corev1.Lifecycle{
		PreStop: &corev1.Handler{
			HTTPGet: &corev1.HTTPGetAction{
				Port: intstr.FromInt(queue.RequestQueueAdminPort),
				Path: queue.RequestQueueQuitPath,
			},
		},
	}
	// If the client provided a readiness check endpoint, we should
	// fill in the port for them so that requests also go through
	// queue proxy for a better health checking logic.
	//
	// TODO(tcnghia): Fail validation webhook when users specify their
	// own port in readiness checks.
	if hasHTTPPath(userContainer.ReadinessProbe) {
		userContainer.ReadinessProbe.Handler.HTTPGet.Port = intstr.FromInt(queue.RequestQueuePort)
	}

	podSpe := &corev1.PodSpec{
		Containers:         []corev1.Container{*userContainer, *MakeElaQueueContainer(rev, controllerConfig)},
		Volumes:            []corev1.Volume{varLogVolume},
		ServiceAccountName: rev.Spec.ServiceAccountName,
	}

	// Add Fluentd sidecar and its config map volume if var log collection is enabled.
	if controllerConfig.EnableVarLogCollection {
		fluentdConfigMapVolume := corev1.Volume{
			Name: fluentdConfigMapVolumeName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: "fluentd-varlog-config",
					},
				},
			},
		}

		fluentdContainer := corev1.Container{
			Name:  fluentdContainerName,
			Image: controllerConfig.FluentdSidecarImage,
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceName("cpu"): resource.MustParse(fluentdContainerCPU),
				},
			},
			Env: []corev1.EnvVar{
				{
					Name:  "FLUENTD_ARGS",
					Value: "--no-supervisor -q",
				},
				{
					Name:  "ELA_CONTAINER_NAME",
					Value: userContainerName,
				},
				{
					Name:  "ELA_CONFIGURATION",
					Value: controller.LookupOwningConfigurationName(rev.OwnerReferences),
				},
				{
					Name:  "ELA_REVISION",
					Value: rev.Name,
				},
				{
					Name:  "ELA_NAMESPACE",
					Value: rev.Namespace,
				},
				{
					Name: "ELA_POD_NAME",
					ValueFrom: &corev1.EnvVarSource{
						FieldRef: &corev1.ObjectFieldSelector{
							FieldPath: "metadata.name",
						},
					},
				},
			},
			VolumeMounts: []corev1.VolumeMount{
				{
					Name:      varLogVolumeName,
					MountPath: "/var/log/revisions",
				},
				{
					Name:      fluentdConfigMapVolumeName,
					MountPath: "/etc/fluent/config.d",
				},
			},
		}

		podSpe.Containers = append(podSpe.Containers, fluentdContainer)
		podSpe.Volumes = append(podSpe.Volumes, fluentdConfigMapVolume)
	}

	return podSpe
}

// MakeElaDeployment creates a deployment.
func MakeElaDeployment(logger *zap.SugaredLogger, u *v1alpha1.Revision, namespace string,
	networkConfig *NetworkConfig) *appsv1.Deployment {
	rollingUpdateConfig := appsv1.RollingUpdateDeployment{
		MaxUnavailable: &elaPodMaxUnavailable,
		MaxSurge:       &elaPodMaxSurge,
	}

	podTemplateAnnotations := MakeElaResourceAnnotations(u)
	podTemplateAnnotations[sidecarIstioInjectAnnotation] = "true"

	// Inject the IP ranges for istio sidecar configuration.
	// We will inject this value only if all of the following are true:
	// - the config map contains a non-empty value
	// - the user doesn't specify this annotation in configuration's pod template
	// - configured values are valid CIDR notation IP addresses
	// If these conditions are not met, this value will be left untouched.
	// * is a special value that is accepted as a valid.
	// * intercepts calls to all IPs: in cluster as well as outside the cluster.
	if _, ok := podTemplateAnnotations[istioOutboundIPRangeAnnotation]; !ok {
		if len(networkConfig.IstioOutboundIPRanges) > 0 {
			if err := validateOutboundIPRanges(networkConfig.IstioOutboundIPRanges); err != nil {
				logger.Errorf("Failed to parse IP ranges %v. Not setting the annotation. Error: %v", networkConfig.IstioOutboundIPRanges, err)
			} else {
				podTemplateAnnotations[istioOutboundIPRangeAnnotation] = networkConfig.IstioOutboundIPRanges
			}
		}
	}

	return &appsv1.Deployment{
		ObjectMeta: meta_v1.ObjectMeta{
			Name:        controller.GetRevisionDeploymentName(u),
			Namespace:   namespace,
			Labels:      MakeElaResourceLabels(u),
			Annotations: MakeElaResourceAnnotations(u),
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &elaPodReplicaCount,
			Selector: MakeElaResourceSelector(u),
			Strategy: appsv1.DeploymentStrategy{
				Type:          "RollingUpdate",
				RollingUpdate: &rollingUpdateConfig,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: meta_v1.ObjectMeta{
					Labels:      MakeElaResourceLabels(u),
					Annotations: podTemplateAnnotations,
				},
			},
		},
	}
}

func validateOutboundIPRanges(s string) error {
	// * is a valid value
	if s == "*" {
		return nil
	}
	cidrs := strings.Split(s, ",")
	for _, cidr := range cidrs {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return err
		}
	}
	return nil
}