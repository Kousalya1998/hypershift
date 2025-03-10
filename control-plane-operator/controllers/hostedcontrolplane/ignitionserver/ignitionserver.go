package ignitionserver

import (
	"bytes"
	"context"
	"fmt"
	"net"

	configv1 "github.com/openshift/api/config/v1"
	routev1 "github.com/openshift/api/route/v1"
	"github.com/openshift/hypershift/api"
	hyperv1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	"github.com/openshift/hypershift/control-plane-operator/controllers/hostedcontrolplane/manifests"
	"github.com/openshift/hypershift/hypershift-operator/controllers/manifests/controlplaneoperator"
	"github.com/openshift/hypershift/hypershift-operator/controllers/manifests/ignitionserver"
	"github.com/openshift/hypershift/support/certs"
	"github.com/openshift/hypershift/support/config"
	"github.com/openshift/hypershift/support/proxy"
	"github.com/openshift/hypershift/support/thirdparty/library-go/pkg/image/reference"
	"github.com/openshift/hypershift/support/upsert"
	"github.com/openshift/hypershift/support/util"
	prometheusoperatorv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	utilpointer "k8s.io/utils/pointer"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func ReconcileIgnitionServer(ctx context.Context,
	c client.Client,
	createOrUpdate upsert.CreateOrUpdateFN,
	releaseVersion string,
	utilitiesImage string,
	componentImages map[string]string,
	hcp *hyperv1.HostedControlPlane,
	defaultIngressDomain string,
	hasHealthzHandler bool,
	registryOverrides map[string]string,
	openShiftRegistryOverrides string,
	managementClusterHasCapabilitySecurityContextConstraint bool,
	ownerRef config.OwnerRef,
	openShiftTrustedCABundleConfigMapExists bool,
	mirroredReleaseImage string,
) error {
	log := ctrl.LoggerFrom(ctx)

	controlPlaneNamespace := hcp.Namespace
	serviceStrategy := util.ServicePublishingStrategyByTypeForHCP(hcp, hyperv1.Ignition)
	if serviceStrategy == nil {
		//lint:ignore ST1005 Ignition is proper name
		return fmt.Errorf("Ignition service strategy not specified")
	}

	var routeServiceName string
	ignitionServerService := ignitionserver.Service(controlPlaneNamespace)
	if hcp.Spec.Platform.Type != hyperv1.IBMCloudPlatform {
		if _, err := createOrUpdate(ctx, c, ignitionServerService, func() error {
			return reconcileIgnitionServerServiceWithProxy(ignitionServerService, serviceStrategy)
		}); err != nil {
			return fmt.Errorf("failed to reconcile ignition service: %w", err)
		}
		ignitionServerProxyService := ignitionserver.ProxyService(controlPlaneNamespace)
		if _, err := createOrUpdate(ctx, c, ignitionServerProxyService, func() error {
			return reconcileIgnitionServerProxyService(ignitionServerProxyService, serviceStrategy)
		}); err != nil {
			return fmt.Errorf("failed to reconcile ignition proxy service: %w", err)
		}
		routeServiceName = ignitionServerProxyService.Name
	} else {
		if _, err := createOrUpdate(ctx, c, ignitionServerService, func() error {
			return reconcileIgnitionServerService(ignitionServerService, serviceStrategy)
		}); err != nil {
			return fmt.Errorf("failed to reconcile ignition service: %w", err)
		}
		routeServiceName = ignitionServerService.Name
	}

	var ignitionServerAddress string
	switch serviceStrategy.Type {
	case hyperv1.Route:
		// Reconcile routes
		ignitionServerRoute := ignitionserver.Route(controlPlaneNamespace)
		if util.IsPrivateHCP(hcp) {
			if _, err := createOrUpdate(ctx, c, ignitionServerRoute, func() error {
				err := reconcileInternalRoute(ignitionServerRoute, ownerRef, routeServiceName)
				if err != nil {
					return fmt.Errorf("failed to reconcile internal route in ignition server: %w", err)
				}
				return nil
			}); err != nil {
				return fmt.Errorf("failed to reconcile ignition internal route: %w", err)
			}
		} else {
			if _, err := createOrUpdate(ctx, c, ignitionServerRoute, func() error {
				hostname := ""
				if serviceStrategy.Route != nil {
					hostname = serviceStrategy.Route.Hostname
				}
				err := reconcileExternalRoute(ignitionServerRoute, ownerRef, routeServiceName, hostname, defaultIngressDomain)
				if err != nil {
					return fmt.Errorf("failed to reconcile external route in ignition server: %w", err)
				}
				return nil
			}); err != nil {
				return fmt.Errorf("failed to reconcile ignition external route: %w", err)
			}
		}

		// The route must be admitted and assigned a host before we can generate certs
		if len(ignitionServerRoute.Status.Ingress) == 0 || len(ignitionServerRoute.Status.Ingress[0].Host) == 0 {
			log.Info("ignition server reconciliation waiting for ignition server route to be assigned a host value")
			return nil
		}
		ignitionServerAddress = ignitionServerRoute.Status.Ingress[0].Host
	case hyperv1.NodePort:
		if serviceStrategy.NodePort == nil {
			return fmt.Errorf("nodeport metadata not specified for ignition service")
		}
		ignitionServerAddress = serviceStrategy.NodePort.Address
	default:
		return fmt.Errorf("unknown service strategy type for ignition service: %s", serviceStrategy.Type)
	}

	_, disablePKIReconciliation := hcp.Annotations[hyperv1.DisablePKIReconciliationAnnotation]
	if !disablePKIReconciliation {
		// Reconcile a root CA for ignition serving certificates.
		// We only create this and don't update it for now.
		caCertSecret := ignitionserver.IgnitionCACertSecret(controlPlaneNamespace)
		if result, err := createOrUpdate(ctx, c, caCertSecret, func() error {
			return reconcileCACertSecret(caCertSecret)
		}); err != nil {
			return fmt.Errorf("failed to reconcile ignition ca cert: %w", err)
		} else {
			log.Info("reconciled ignition CA cert secret", "result", result)
		}

		// Reconcile an ignition serving certificate issued by the generated root CA.
		// We only create this and don't update it for now.
		servingCertSecret := ignitionserver.IgnitionServingCertSecret(controlPlaneNamespace)
		if result, err := createOrUpdate(ctx, c, servingCertSecret, func() error {
			return reconcileServingCertSecret(servingCertSecret, caCertSecret, ignitionServerAddress)
		}); err != nil {
			return fmt.Errorf("failed to reconcile ignition serving cert: %w", err)
		} else {
			log.Info("reconciled ignition serving cert secret", "result", result)
		}
	}

	role := ignitionserver.Role(controlPlaneNamespace)
	if result, err := createOrUpdate(ctx, c, role, func() error {
		return reconcileRole(role)
	}); err != nil {
		return fmt.Errorf("failed to reconcile ignition server role: %w", err)
	} else {
		log.Info("Reconciled ignition server role", "result", result)
	}

	sa := ignitionserver.ServiceAccount(controlPlaneNamespace)
	if result, err := createOrUpdate(ctx, c, sa, func() error {
		util.EnsurePullSecret(sa, controlplaneoperator.PullSecret("").Name)
		return nil
	}); err != nil {
		return fmt.Errorf("failed to reconcile ignition server service account: %w", err)
	} else {
		log.Info("Reconciled ignition server service account", "result", result)
	}

	roleBinding := ignitionserver.RoleBinding(controlPlaneNamespace)
	if result, err := createOrUpdate(ctx, c, roleBinding, func() error {
		return reconcileRoleBinding(roleBinding, role, sa)
	}); err != nil {
		return fmt.Errorf("failed to reconcile ignition server role binding: %w", err)
	} else {
		log.Info("Reconciled ignition server role binding", "result", result)
	}

	ignitionServerLabels := map[string]string{
		"app":                         ignitionserver.ResourceName,
		hyperv1.ControlPlaneComponent: ignitionserver.ResourceName,
	}
	configOperatorImage := componentImages["cluster-config-operator"]
	if configOperatorImage == "" {
		return fmt.Errorf("cluster-config-operator image not found in payload images")
	}
	machineConfigOperatorImage := componentImages["machine-config-operator"]
	if machineConfigOperatorImage == "" {
		return fmt.Errorf("machine-config-operator image not found in payload images")
	}
	servingCertSecretName := ignitionserver.IgnitionServingCertSecret("").Name
	if hcp.Spec.Platform.Type != hyperv1.IBMCloudPlatform {
		servingCertSecretName = manifests.IgnitionServerCertSecret("").Name
	}

	// Determine if we need to override the machine config operator and cluster config operator
	// images based on image mappings present in management cluster.
	ocpRegistryMapping := util.ConvertImageRegistryOverrideStringToMap(openShiftRegistryOverrides)
	overrideConfigOperatorImage, err := lookupMappedImage(ocpRegistryMapping, configOperatorImage)
	if err != nil {
		return err
	}
	overrideMachineConfigOperatorImage, err := lookupMappedImage(ocpRegistryMapping, machineConfigOperatorImage)
	if err != nil {
		return err
	}

	imageOverrides := map[string]string{}
	for k, v := range registryOverrides {
		if k != "" {
			imageOverrides[k] = v
		}
	}

	if overrideConfigOperatorImage != configOperatorImage {
		imageOverrides[configOperatorImage] = overrideConfigOperatorImage
	}

	if overrideMachineConfigOperatorImage != machineConfigOperatorImage {
		imageOverrides[machineConfigOperatorImage] = overrideMachineConfigOperatorImage
	}

	ignitionServerDeployment := ignitionserver.Deployment(controlPlaneNamespace)
	if result, err := createOrUpdate(ctx, c, ignitionServerDeployment, func() error {
		return reconcileDeployment(ignitionServerDeployment,
			releaseVersion,
			utilitiesImage,
			configOperatorImage,
			hcp,
			defaultIngressDomain,
			hasHealthzHandler,
			imageOverrides,
			openShiftRegistryOverrides,
			managementClusterHasCapabilitySecurityContextConstraint,
			ignitionServerLabels,
			servingCertSecretName,
			openShiftTrustedCABundleConfigMapExists,
			mirroredReleaseImage,
		)
	}); err != nil {
		return fmt.Errorf("failed to reconcile ignition deployment: %w", err)
	} else {
		log.Info("Reconciled ignition server deployment", "result", result)
	}

	podMonitor := ignitionserver.PodMonitor(controlPlaneNamespace)
	if result, err := createOrUpdate(ctx, c, podMonitor, func() error {
		return reconcilePodMonitor(podMonitor,
			ownerRef,
			ignitionServerLabels,
			controlPlaneNamespace,
			hcp.Spec.ClusterID)
	}); err != nil {
		return fmt.Errorf("failed to reconcile ignition server pod monitor: %w", err)
	} else {
		log.Info("Reconciled ignition server podmonitor", "result", result)
	}

	if hcp.Spec.Platform.Type != hyperv1.IBMCloudPlatform {
		haproxyImage := componentImages["haproxy-router"]
		if haproxyImage == "" {
			return fmt.Errorf("haproxy-router image not found in payload images")
		}
		ignitionServerProxyDeployment := ignitionserver.ProxyDeployment(controlPlaneNamespace)
		if result, err := createOrUpdate(ctx, c, ignitionServerProxyDeployment, func() error {
			return reconcileProxyDeployment(ignitionServerProxyDeployment,
				hcp,
				haproxyImage,
				managementClusterHasCapabilitySecurityContextConstraint)
		}); err != nil {
			return fmt.Errorf("failed to reconcile ignition proxy deployment: %w", err)
		} else {
			log.Info("Reconciled ignition server proxy deployment", "result", result)
		}

		role := ignitionserver.ProxyRole(controlPlaneNamespace)
		sa := ignitionserver.ProxyServiceAccount(controlPlaneNamespace)
		roleBinding := ignitionserver.ProxyRoleBinding(controlPlaneNamespace)

		for _, resource := range []client.Object{role, sa, roleBinding} {
			if _, err := util.DeleteIfNeeded(ctx, c, resource); err != nil {
				log.Error(err, "Failed to delete deprecated resource", "resource", client.ObjectKeyFromObject(resource).String())
			}
		}
	}

	return nil
}

func reconcileIgnitionServerService(svc *corev1.Service, strategy *hyperv1.ServicePublishingStrategy) error {
	return reconcileIgnitionExternalService(svc, strategy, false)
}

func reconcileIgnitionServerProxyService(svc *corev1.Service, strategy *hyperv1.ServicePublishingStrategy) error {
	return reconcileIgnitionExternalService(svc, strategy, true)
}

func reconcileIgnitionExternalService(svc *corev1.Service, strategy *hyperv1.ServicePublishingStrategy, isProxy bool) error {
	appLabel := ignitionserver.ResourceName
	targetPort := intstr.FromInt(9090)
	if isProxy {
		appLabel = "ignition-server-proxy"
		targetPort = intstr.FromString("https")
	}

	svc.Spec.Selector = map[string]string{
		"app": appLabel,
	}
	var portSpec corev1.ServicePort
	if len(svc.Spec.Ports) > 0 {
		portSpec = svc.Spec.Ports[0]
	} else {
		svc.Spec.Ports = []corev1.ServicePort{portSpec}
	}
	portSpec.Port = int32(443)
	portSpec.Name = "https"
	portSpec.Protocol = corev1.ProtocolTCP
	portSpec.TargetPort = targetPort
	switch strategy.Type {
	case hyperv1.NodePort:
		svc.Spec.Type = corev1.ServiceTypeNodePort
		if portSpec.NodePort == 0 && strategy.NodePort != nil {
			portSpec.NodePort = strategy.NodePort.Port
		}
	case hyperv1.Route:
		svc.Spec.Type = corev1.ServiceTypeClusterIP
	default:
		return fmt.Errorf("invalid publishing strategy for Ignition service: %s", strategy.Type)
	}
	svc.Spec.Ports[0] = portSpec
	return nil
}

func reconcileIgnitionServerServiceWithProxy(svc *corev1.Service, strategy *hyperv1.ServicePublishingStrategy) error {
	svc.Spec.Selector = map[string]string{
		"app": ignitionserver.ResourceName,
	}
	svc.Spec.Type = corev1.ServiceTypeClusterIP
	svc.Spec.Ports = []corev1.ServicePort{
		{
			Port:       int32(443),
			Name:       "https",
			Protocol:   corev1.ProtocolTCP,
			TargetPort: intstr.FromInt(9090),
		},
	}
	return nil
}

func reconcileExternalRoute(route *routev1.Route, ownerRef config.OwnerRef, svcName string, hostname string, defaultIngressDomain string) error {
	ownerRef.ApplyTo(route)
	return util.ReconcileExternalRoute(route, hostname, defaultIngressDomain, svcName)
}

func reconcileInternalRoute(route *routev1.Route, ownerRef config.OwnerRef, svcName string) error {
	ownerRef.ApplyTo(route)
	// Assumes ownerRef is the HCP
	return util.ReconcileInternalRoute(route, ownerRef.Reference.Name, svcName)
}

func reconcileCACertSecret(caCertSecret *corev1.Secret) error {
	caCertSecret.Type = corev1.SecretTypeTLS
	return certs.ReconcileSelfSignedCA(caCertSecret, "ignition-root-ca", "openshift", func(o *certs.CAOpts) {
		o.CASignerCertMapKey = corev1.TLSCertKey
		o.CASignerKeyMapKey = corev1.TLSPrivateKeyKey
	})
}

func reconcileServingCertSecret(servingCertSecret *corev1.Secret, caCertSecret *corev1.Secret, ignitionServerAddress string) error {
	servingCertSecret.Type = corev1.SecretTypeTLS

	var dnsNames, ipAddresses []string
	numericIP := net.ParseIP(ignitionServerAddress)
	if numericIP == nil {
		dnsNames = []string{ignitionServerAddress}
	} else {
		ipAddresses = []string{ignitionServerAddress}
	}

	return certs.ReconcileSignedCert(
		servingCertSecret,
		caCertSecret,
		"ignition-server",
		[]string{"openshift"},
		nil,
		corev1.TLSCertKey,
		corev1.TLSPrivateKeyKey,
		"",
		dnsNames,
		ipAddresses,
		func(o *certs.CAOpts) {
			o.CASignerCertMapKey = corev1.TLSCertKey
			o.CASignerKeyMapKey = corev1.TLSPrivateKeyKey
		},
	)
}

func reconcileRole(role *rbacv1.Role) error {
	role.Rules = []rbacv1.PolicyRule{
		{
			APIGroups: []string{""},
			Resources: []string{
				"secrets",
			},
			Verbs: []string{"get", "list", "watch", "update", "patch", "delete"},
		},
		{
			APIGroups: []string{""},
			Resources: []string{
				"configmaps",
			},
			Verbs: []string{"get", "list", "watch"},
		},
		{
			APIGroups: []string{""},
			Resources: []string{
				"events",
			},
			Verbs: []string{"*"},
		},
		{
			APIGroups: []string{hyperv1.GroupVersion.Group},
			Resources: []string{
				"hostedcontrolplanes",
			},
			Verbs: []string{
				"get",
				"list",
				"watch",
			},
		},
		{
			APIGroups: []string{hyperv1.GroupVersion.Group},
			Resources: []string{
				"hostedcontrolplanes/status",
			},
			Verbs: []string{"*"},
		},
	}
	return nil
}

func reconcileRoleBinding(roleBinding *rbacv1.RoleBinding, role *rbacv1.Role, sa *corev1.ServiceAccount) error {
	roleBinding.RoleRef = rbacv1.RoleRef{
		APIGroup: "rbac.authorization.k8s.io",
		Kind:     "Role",
		Name:     role.Name,
	}

	roleBinding.Subjects = []rbacv1.Subject{
		{
			Kind:      "ServiceAccount",
			Name:      sa.Name,
			Namespace: sa.Namespace,
		},
	}
	return nil
}

func reconcileDeployment(deployment *appsv1.Deployment,
	releaseVersion string,
	utilitiesImage string,
	configOperatorImage string,
	hcp *hyperv1.HostedControlPlane,
	defaultIngressDomain string,
	hasHealthzHandler bool,
	registryOverrides map[string]string,
	openShiftRegistryOverrides string,
	managementClusterHasCapabilitySecurityContextConstraint bool,
	ignitionServerLabels map[string]string,
	servingCertSecretName string,
	openShiftTrustedCABundleConfigMapForCPOExists bool,
	mirroredReleaseImage string,
) error {
	var probeHandler corev1.ProbeHandler
	if hasHealthzHandler {
		probeHandler.HTTPGet = &corev1.HTTPGetAction{
			Path:   "/healthz",
			Port:   intstr.FromInt(9090),
			Scheme: corev1.URISchemeHTTPS,
		}
	} else {
		probeHandler.TCPSocket = &corev1.TCPSocketAction{
			Port: intstr.FromInt(9090),
		}
	}

	// Use the default FeatureSet unless otherwise specified.
	featureGate := &configv1.FeatureGate{
		TypeMeta: metav1.TypeMeta{
			Kind:       "FeatureGate",
			APIVersion: configv1.GroupVersion.String(),
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "cluster",
		},
	}
	if hcp.Spec.Configuration != nil && hcp.Spec.Configuration.FeatureGate != nil {
		featureGate.Spec = *hcp.Spec.Configuration.FeatureGate
	}

	featureGateBuffer := &bytes.Buffer{}
	if err := api.YamlSerializer.Encode(featureGate, featureGateBuffer); err != nil {
		return fmt.Errorf("failed to encode feature gates: %w", err)
	}
	featureGateYAML := featureGateBuffer.String()

	if deployment.Annotations == nil {
		deployment.Annotations = map[string]string{}
	}

	// Before this change we did
	// 		Selector: &metav1.LabelSelector{
	//			MatchLabels: ignitionServerLabels,
	//		},
	//		Template: corev1.PodTemplateSpec{
	//			ObjectMeta: metav1.ObjectMeta{
	//				Labels: ignitionServerLabels,
	//			}
	// As a consequence of using the same memory address for both MatchLabels and Labels, when setColocation set the colocationLabelKey in additionalLabels
	// it got also silently included in MatchLabels. This made any additional additionalLabel to break reconciliation because MatchLabels is an immutable field.
	// So now we leave Selector.MatchLabels if it has something already and use a different var from .Labels so the former is not impacted by additionalLabels changes.
	selectorLabels := ignitionServerLabels
	if deployment.Spec.Selector != nil && deployment.Spec.Selector.MatchLabels != nil {
		selectorLabels = deployment.Spec.Selector.MatchLabels
	}

	deployment.Spec = appsv1.DeploymentSpec{
		Selector: &metav1.LabelSelector{
			MatchLabels: selectorLabels,
		},
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				// We copy the map here, otherwise this .Labels would point to the same address that .MatchLabels
				// Then when additionalLabels are applied it silently modifies .MatchLabels.
				// We could also change additionalLabels.ApplyTo but that might have a bigger impact.
				// TODO (alberto): Refactor support.config package and gate all components definition on the library.
				Labels: config.CopyStringMap(ignitionServerLabels),
			},
			Spec: corev1.PodSpec{
				ServiceAccountName:            ignitionserver.ServiceAccount("").Name,
				TerminationGracePeriodSeconds: utilpointer.Int64(10),
				Tolerations: []corev1.Toleration{
					{
						Key:    "node-role.kubernetes.io/master",
						Effect: corev1.TaintEffectNoSchedule,
					},
				},
				Volumes: []corev1.Volume{
					{
						Name: "serving-cert",
						VolumeSource: corev1.VolumeSource{
							Secret: &corev1.SecretVolumeSource{
								SecretName:  servingCertSecretName,
								DefaultMode: utilpointer.Int32(0640),
							},
						},
					},
					{
						Name: "payloads",
						VolumeSource: corev1.VolumeSource{
							EmptyDir: &corev1.EmptyDirVolumeSource{},
						},
					},
					{
						Name: "bootstrap-manifests",
						VolumeSource: corev1.VolumeSource{
							EmptyDir: &corev1.EmptyDirVolumeSource{},
						},
					},
					{
						Name: "manifests",
						VolumeSource: corev1.VolumeSource{
							EmptyDir: &corev1.EmptyDirVolumeSource{},
						},
					},

					{
						Name: "shared",
						VolumeSource: corev1.VolumeSource{
							EmptyDir: &corev1.EmptyDirVolumeSource{},
						},
					},
				},
				InitContainers: []corev1.Container{
					{
						Name:            "fetch-feature-gate",
						Image:           configOperatorImage,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Command: []string{
							"/bin/bash",
						},
						Args: []string{
							"-c",
							invokeFeatureGateRenderScript("/shared", string(featureGateYAML)),
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceMemory: resource.MustParse("40Mi"),
								corev1.ResourceCPU:    resource.MustParse("10m"),
							},
						},
						VolumeMounts: []corev1.VolumeMount{
							{
								Name:      "shared",
								MountPath: "/shared",
							},
						},
					},
				},
				Containers: []corev1.Container{
					{
						Name:            ignitionserver.ResourceName,
						Image:           utilitiesImage,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Env: []corev1.EnvVar{
							{
								Name: "MY_NAMESPACE",
								ValueFrom: &corev1.EnvVarSource{
									FieldRef: &corev1.ObjectFieldSelector{
										FieldPath: "metadata.namespace",
									},
								},
							},
							{
								Name:  "OPENSHIFT_IMG_OVERRIDES",
								Value: openShiftRegistryOverrides,
							},
						},
						Command: []string{
							"/usr/bin/control-plane-operator",
							"ignition-server",
							"--cert-file", "/var/run/secrets/ignition/serving-cert/tls.crt",
							"--key-file", "/var/run/secrets/ignition/serving-cert/tls.key",
							"--registry-overrides", util.ConvertRegistryOverridesToCommandLineFlag(registryOverrides),
							"--platform", string(hcp.Spec.Platform.Type),
							"--feature-gate-manifest=/shared/99_feature-gate.yaml",
						},
						LivenessProbe: &corev1.Probe{
							ProbeHandler:        probeHandler,
							InitialDelaySeconds: 120,
							TimeoutSeconds:      5,
							PeriodSeconds:       60,
							FailureThreshold:    6,
							SuccessThreshold:    1,
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler:        probeHandler,
							InitialDelaySeconds: 5,
							TimeoutSeconds:      5,
							PeriodSeconds:       60,
							FailureThreshold:    3,
							SuccessThreshold:    1,
						},
						Ports: []corev1.ContainerPort{
							{
								Name:          "https",
								ContainerPort: 9090,
							},
							{
								Name:          "metrics",
								ContainerPort: 8080,
							},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceMemory: resource.MustParse("40Mi"),
								corev1.ResourceCPU:    resource.MustParse("10m"),
							},
						},
						VolumeMounts: []corev1.VolumeMount{
							{
								Name:      "serving-cert",
								MountPath: "/var/run/secrets/ignition/serving-cert",
							},
							{
								Name:      "payloads",
								MountPath: "/payloads",
							},
							{
								Name:      "bootstrap-manifests",
								MountPath: "/usr/share/bootkube/manifests/bootstrap-manifests",
							},
							{
								Name:      "manifests",
								MountPath: "/usr/share/bootkube/manifests/manifests",
							},
							{
								Name:      "shared",
								MountPath: "/shared",
							},
						},
					},
				},
			},
		},
	}
	proxy.SetEnvVars(&deployment.Spec.Template.Spec.Containers[0].Env)

	if len(mirroredReleaseImage) > 0 {
		deployment.Spec.Template.Spec.Containers[0].Env = append(deployment.Spec.Template.Spec.Containers[0].Env, corev1.EnvVar{Name: "MIRRORED_RELEASE_IMAGE", Value: mirroredReleaseImage})
	}

	if hcp.Spec.AdditionalTrustBundle != nil {
		// Add trusted-ca mount with optional configmap
		util.DeploymentAddTrustBundleVolume(hcp.Spec.AdditionalTrustBundle, deployment)
	}

	if openShiftTrustedCABundleConfigMapForCPOExists {
		util.DeploymentAddOpenShiftTrustedCABundleConfigMap(deployment)
	}

	// set security context
	if !managementClusterHasCapabilitySecurityContextConstraint {
		deployment.Spec.Template.Spec.SecurityContext = &corev1.PodSecurityContext{
			RunAsUser: utilpointer.Int64(config.DefaultSecurityContextUser),
		}
	}

	deploymentConfig := config.DeploymentConfig{
		AdditionalLabels: map[string]string{
			config.NeedManagementKASAccessLabel: "true",
		},
	}
	deploymentConfig.Scheduling.PriorityClass = config.DefaultPriorityClass
	deploymentConfig.SetRestartAnnotation(hcp.ObjectMeta)
	deploymentConfig.SetDefaults(hcp, ignitionServerLabels, nil)
	deploymentConfig.ApplyTo(deployment)

	return nil
}

func reconcileProxyDeployment(deployment *appsv1.Deployment, hcp *hyperv1.HostedControlPlane, haproxyImage string, managementClusterHasCapabilitySecurityContextConstraint bool) error {
	commandScript := `#!/bin/bash
set -e
cat /etc/ssl/serving-cert/tls.crt /etc/ssl/serving-cert/tls.key > /tmp/tls.pem
cat <<EOF > /tmp/haproxy.conf
defaults
  mode http
  timeout connect 5s
  timeout client 30s
  timeout server 30s

frontend ignition-server
  bind :::8443 v4v6 ssl crt /tmp/tls.pem
  default_backend ignition_servers

backend ignition_servers
  server ignition-server ignition-server:443 check ssl ca-file /etc/ssl/root-ca/ca.crt
EOF
haproxy -f /tmp/haproxy.conf
`

	deployment.Spec = appsv1.DeploymentSpec{
		Selector: &metav1.LabelSelector{
			MatchLabels: map[string]string{
				"app": "ignition-server-proxy",
			},
		},
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					"app": "ignition-server-proxy",
				},
			},
			Spec: corev1.PodSpec{
				AutomountServiceAccountToken: utilpointer.Bool(false),
				Containers: []corev1.Container{
					{
						Name:            "haproxy",
						Image:           haproxyImage,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Command: []string{
							"/bin/bash",
						},
						Args: []string{
							"-c",
							commandScript,
						},

						Ports: []corev1.ContainerPort{
							{
								Name:          "https",
								ContainerPort: 8443,
							},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceMemory: resource.MustParse("20Mi"),
								corev1.ResourceCPU:    resource.MustParse("10m"),
							},
						},
						SecurityContext: &corev1.SecurityContext{
							Capabilities: &corev1.Capabilities{
								Add: []corev1.Capability{
									"NET_BIND_SERVICE",
								},
							},
						},
						VolumeMounts: []corev1.VolumeMount{
							{
								Name:      "serving-cert",
								MountPath: "/etc/ssl/serving-cert",
							},
							{
								Name:      "root-ca",
								MountPath: "/etc/ssl/root-ca",
							},
						},
					},
				},
				ServiceAccountName: "",
				Volumes: []corev1.Volume{
					{
						Name: "serving-cert",
						VolumeSource: corev1.VolumeSource{
							Secret: &corev1.SecretVolumeSource{
								SecretName:  ignitionserver.IgnitionServingCertSecret("").Name,
								DefaultMode: utilpointer.Int32(0640),
							},
						},
					},
					{
						Name: "root-ca",
						VolumeSource: corev1.VolumeSource{
							ConfigMap: &corev1.ConfigMapVolumeSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: manifests.RootCAConfigMap("").Name},
							},
						},
					},
				},
			},
		},
	}
	proxy.SetEnvVars(&deployment.Spec.Template.Spec.Containers[0].Env)

	if hcp.Spec.AdditionalTrustBundle != nil {
		// Add trusted-ca mount with optional configmap
		util.DeploymentAddTrustBundleVolume(hcp.Spec.AdditionalTrustBundle, deployment)
	}

	// set security context
	if !managementClusterHasCapabilitySecurityContextConstraint {
		deployment.Spec.Template.Spec.SecurityContext = &corev1.PodSecurityContext{
			RunAsUser: utilpointer.Int64(config.DefaultSecurityContextUser),
		}
	}

	deploymentConfig := config.DeploymentConfig{}
	deploymentConfig.Scheduling.PriorityClass = config.DefaultPriorityClass
	if hcp.Annotations[hyperv1.ControlPlanePriorityClass] != "" {
		deploymentConfig.Scheduling.PriorityClass = hcp.Annotations[hyperv1.ControlPlanePriorityClass]
	}
	deploymentConfig.SetRestartAnnotation(hcp.ObjectMeta)
	deploymentConfig.SetRequestServingDefaults(hcp, map[string]string{"app": "ignition-server-proxy"}, nil)
	deploymentConfig.ApplyTo(deployment)

	return nil
}

func reconcilePodMonitor(podMonitor *prometheusoperatorv1.PodMonitor,
	ownerRef config.OwnerRef,
	ignitionServerLabels map[string]string,
	controlPlaneNamespace string,
	clusterID string) error {
	ownerRef.ApplyTo(podMonitor)
	podMonitor.Spec.Selector = metav1.LabelSelector{
		MatchLabels: ignitionServerLabels,
	}
	podMonitor.Spec.PodMetricsEndpoints = []prometheusoperatorv1.PodMetricsEndpoint{{
		Port: "metrics",
	}}
	podMonitor.Spec.NamespaceSelector = prometheusoperatorv1.NamespaceSelector{MatchNames: []string{controlPlaneNamespace}}
	if podMonitor.Annotations == nil {
		podMonitor.Annotations = map[string]string{}
	}
	util.ApplyClusterIDLabelToPodMonitor(&podMonitor.Spec.PodMetricsEndpoints[0], clusterID)
	return nil
}

// invokeFeatureGateRenderScript writes the passed yaml to disk in a given dir.
func invokeFeatureGateRenderScript(workDir, featureGateYAML string) string {
	var script = `#!/bin/bash
set -e
cd /tmp
mkdir input output manifests

touch /tmp/manifests/99_feature-gate.yaml
cat <<EOF >/tmp/manifests/99_feature-gate.yaml
%[2]s
EOF

cp /tmp/manifests/99_feature-gate.yaml %[1]s/99_feature-gate.yaml
`
	return fmt.Sprintf(script, workDir, featureGateYAML)
}

func lookupMappedImage(ocpOverrides map[string][]string, image string) (string, error) {
	ref, err := reference.Parse(image)
	if err != nil {
		return "", fmt.Errorf("failed to parse image (%s): %w", image, err)
	}
	for source, replacements := range ocpOverrides {
		if ref.AsRepository().String() == source {
			newRef := fmt.Sprintf("%s@%s", replacements[0], ref.ID)
			return newRef, nil
		}
	}
	return image, nil
}
