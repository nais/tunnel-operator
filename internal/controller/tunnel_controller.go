package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	v1alpha1 "github.com/nais/tunnel-operator/api/v1alpha1"
	operatorgrpc "github.com/nais/tunnel-operator/internal/grpc"
	forwarderv1 "github.com/nais/tunnel-operator/pkg/forwarder/proto/forwarder/v1"
	"github.com/nais/tunnel-operator/pkg/portalloc"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	finalizerName     = "tunnels.nais.io/cleanup"
	gatewayStatusPort = 8080
)

type TunnelReconciler struct {
	Client              client.Client
	Scheme              *runtime.Scheme
	PortAllocator       *portalloc.PortAllocator
	ForwarderServer     *operatorgrpc.ForwarderServer
	ForwarderServiceKey client.ObjectKey
	FetchGatewayStatus  func(podIP string) (string, error)
}

//+kubebuilder:rbac:groups=nais.io,resources=tunnels,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=nais.io,resources=tunnels/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=nais.io,resources=tunnels/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;delete
//+kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch
//+kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;delete

func (r *TunnelReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, retErr error) {
	defer func() {
		if retErr != nil {
			reconciliationsTotal.WithLabelValues("error").Inc()
		} else {
			reconciliationsTotal.WithLabelValues("success").Inc()
		}
	}()

	logger := log.FromContext(ctx)

	tunnel := &v1alpha1.Tunnel{}
	if err := r.Client.Get(ctx, req.NamespacedName, tunnel); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !tunnel.DeletionTimestamp.IsZero() {
		if err := r.handleDeletion(ctx, tunnel); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if tunnel.Status.Phase == v1alpha1.TunnelPhaseTerminated {
		logger.Info("cleaning up terminated tunnel", "tunnel", req.NamespacedName)
		if err := r.handleTerminated(ctx, tunnel); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(tunnel, finalizerName) {
		controllerutil.AddFinalizer(tunnel, finalizerName)
		return ctrl.Result{}, r.Client.Update(ctx, tunnel)
	}

	resourceName := gatewayResourceName(tunnel.Name)
	labels := gatewayLabels(tunnel.Name)
	gatewayImage := os.Getenv("GATEWAY_IMAGE")
	if gatewayImage == "" {
		gatewayImage = "ghcr.io/nais/tunnel-operator/gateway:latest"
	}
	gatewayDebug := os.Getenv("GATEWAY_DEBUG") == "true"

	deadlineSeconds := int64(3600)
	if tunnel.Spec.ActiveDeadlineSeconds != nil {
		deadlineSeconds = *tunnel.Spec.ActiveDeadlineSeconds
	}

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: tunnel.Namespace}}
	if err := r.Client.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("getting gateway pod: %w", err)
		}
		pod.Labels = labels
		if gatewayDebug {
			pod.Labels["kyverno.policy.exclusion.nais.io/disallow-capabilities-strict"] = "true"
		}
		pod.Annotations = map[string]string{
			"prometheus.io/scrape": "true",
			"prometheus.io/port":   "9091",
			"prometheus.io/path":   "/metrics",
		}
		pod.Spec = corev1.PodSpec{
			RestartPolicy:         corev1.RestartPolicyNever,
			ActiveDeadlineSeconds: &deadlineSeconds,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: new(true),
				RunAsUser:    new(int64(65532)),
				SeccompProfile: &corev1.SeccompProfile{
					Type: corev1.SeccompProfileTypeRuntimeDefault,
				},
			},
			Containers: []corev1.Container{{
				Name:  "gateway",
				Image: gatewayImage,
				Ports: []corev1.ContainerPort{
					{
						Name:          "status",
						ContainerPort: int32(gatewayStatusPort),
						Protocol:      corev1.ProtocolTCP,
					},
					{
						Name:          "metrics",
						ContainerPort: 9091,
						Protocol:      corev1.ProtocolTCP,
					},
				},
				Env: []corev1.EnvVar{
					{Name: "TUNNEL_PEER_PUBLIC_KEY", Value: tunnel.Spec.ClientPublicKey},
					{Name: "TUNNEL_TARGET_HOST", Value: tunnel.Spec.Target.Host},
					{Name: "TUNNEL_TARGET_PORT", Value: strconv.Itoa(int(tunnel.Spec.Target.Port))},
					{Name: "TUNNEL_NAME", Value: tunnel.Name},
					{Name: "TUNNEL_NAMESPACE", Value: tunnel.Namespace},
					{Name: "LOG_LEVEL", Value: os.Getenv("LOG_LEVEL")},
				},
				ReadinessProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{
						HTTPGet: &corev1.HTTPGetAction{
							Path: "/status",
							Port: intstr.FromInt32(int32(gatewayStatusPort)),
						},
					},
					InitialDelaySeconds: 1,
					PeriodSeconds:       2,
				},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: new(false),
					RunAsNonRoot:             new(true),
					RunAsUser:                new(int64(65532)),
					ReadOnlyRootFilesystem:   new(true),
					SeccompProfile: &corev1.SeccompProfile{
						Type: corev1.SeccompProfileTypeRuntimeDefault,
					},
					Capabilities: gatewayCapabilities(gatewayDebug),
				},
			}},
		}
		if err := controllerutil.SetControllerReference(tunnel, pod, r.Scheme); err != nil {
			return ctrl.Result{}, fmt.Errorf("setting owner reference on pod: %w", err)
		}
		if err := r.Client.Create(ctx, pod); err != nil {
			return ctrl.Result{}, fmt.Errorf("creating gateway pod: %w", err)
		}
	}

	networkPolicy := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: tunnel.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, networkPolicy, func() error {
		networkPolicy.Labels = labels
		networkPolicy.Spec = networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{"tunnels.nais.io/tunnel": tunnel.Name},
			},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{
					Ports: []networkingv1.NetworkPolicyPort{
						{
							Protocol: new(corev1.ProtocolUDP),
							Port:     intstrPtr(51820),
						},
						{
							Protocol: new(corev1.ProtocolTCP),
							Port:     intstrPtr(int32(gatewayStatusPort)),
						},
						{
							Protocol: new(corev1.ProtocolTCP),
							Port:     intstrPtr(9091),
						},
					},
				},
			},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					To: []networkingv1.NetworkPolicyPeer{{
						IPBlock: &networkingv1.IPBlock{CIDR: tunnel.Spec.Target.ResolvedIP + "/32"},
					}},
					Ports: []networkingv1.NetworkPolicyPort{{
						Port:     intstrPtr(tunnel.Spec.Target.Port),
						Protocol: new(corev1.ProtocolTCP),
					}},
				},
				{
					Ports: []networkingv1.NetworkPolicyPort{{
						Protocol: new(corev1.ProtocolUDP),
					}},
				},
			},
		}
		return controllerutil.SetControllerReference(tunnel, networkPolicy, r.Scheme)
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling networkpolicy: %w", err)
	}

	statusBase := tunnel.DeepCopy()
	updated := false
	var requeueAfter time.Duration
	updateType := forwarderv1.UpdateType_MODIFIED
	if tunnel.Status.ForwarderPort == 0 && r.PortAllocator != nil {
		allocatedPort, err := r.PortAllocator.Allocate(tunnelKey(tunnel))
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("allocating forwarder port: %w", err)
		}
		tunnel.Status.ForwarderPort = allocatedPort
		portAllocationsActive.Inc()
		updated = true
		updateType = forwarderv1.UpdateType_ADDED
	}
	if tunnel.Status.GatewayPodName != resourceName {
		tunnel.Status.GatewayPodName = resourceName
		updated = true
	}

	switch {
	case pod.Status.Phase == corev1.PodFailed:
		if tunnel.Status.Phase != v1alpha1.TunnelPhaseFailed {
			tunnel.Status.Phase = v1alpha1.TunnelPhaseFailed
			tunnel.Status.Message = "Gateway pod failed"
			updated = true
		}
	case pod.Status.Phase == corev1.PodSucceeded:
		if tunnel.Status.Phase != v1alpha1.TunnelPhaseTerminated {
			tunnel.Status.Phase = v1alpha1.TunnelPhaseTerminated
			tunnel.Status.Message = "Gateway terminated"
			updated = true
		}
	case isPodReady(pod):
		fetcher := r.FetchGatewayStatus
		if fetcher == nil {
			fetcher = defaultFetchGatewayStatus
		}
		if pod.Status.PodIP != "" {
			pubKey, err := fetcher(pod.Status.PodIP)
			if err != nil {
				logger.Info("gateway status not yet available", "err", err)
				requeueAfter = 2 * time.Second
			} else if pubKey != "" {
				if tunnel.Status.GatewayPublicKey != pubKey {
					tunnel.Status.GatewayPublicKey = pubKey
					updated = true
				}
				if tunnel.Status.Phase != v1alpha1.TunnelPhaseReady {
					tunnel.Status.Phase = v1alpha1.TunnelPhaseReady
					tunnel.Status.Message = "Gateway ready"
					updated = true
				}
			}
		}
		vip, err := r.resolveForwarderVIP(ctx)
		if err == nil && vip != "" {
			forwarderEndpoint := net.JoinHostPort(vip, strconv.Itoa(int(tunnel.Status.ForwarderPort)))
			if tunnel.Status.ForwarderEndpoint != forwarderEndpoint {
				tunnel.Status.ForwarderEndpoint = forwarderEndpoint
				updated = true
			}
		} else if err != nil {
			logger.Info("forwarder VIP not yet available", "err", err)
			requeueAfter = 2 * time.Second
		}
	default:
		if tunnel.Status.Phase == "" || tunnel.Status.Phase == v1alpha1.TunnelPhasePending {
			tunnel.Status.Phase = v1alpha1.TunnelPhaseProvisioning
			tunnel.Status.Message = "Gateway pod starting"
			updated = true
		}
	}

	if updated {
		if err := r.Client.Status().Patch(ctx, tunnel, client.MergeFrom(statusBase)); err != nil {
			return ctrl.Result{}, fmt.Errorf("updating tunnel status: %w", err)
		}
		tunnelsActive.WithLabelValues(string(tunnel.Status.Phase), tunnel.Namespace).Inc()
		r.notifyTunnelUpdate(ctx, tunnel, updateType)
	}

	logger.Info("reconciled tunnel", "tunnel", req.NamespacedName, "pod", resourceName)

	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

func (r *TunnelReconciler) handleDeletion(ctx context.Context, tunnel *v1alpha1.Tunnel) error {
	if !controllerutil.ContainsFinalizer(tunnel, finalizerName) {
		r.releaseForwarderPort(tunnel)
		return nil
	}

	if err := r.deleteGatewayResources(ctx, tunnel); err != nil {
		return err
	}

	controllerutil.RemoveFinalizer(tunnel, finalizerName)
	if err := r.Client.Update(ctx, tunnel); err != nil {
		return fmt.Errorf("removing finalizer: %w", err)
	}

	r.releaseForwarderPort(tunnel)
	r.notifyTunnelUpdate(ctx, tunnel, forwarderv1.UpdateType_DELETED)

	return nil
}

func (r *TunnelReconciler) handleTerminated(ctx context.Context, tunnel *v1alpha1.Tunnel) error {
	if err := r.deleteGatewayResources(ctx, tunnel); err != nil {
		return err
	}

	if controllerutil.ContainsFinalizer(tunnel, finalizerName) {
		controllerutil.RemoveFinalizer(tunnel, finalizerName)
		if err := r.Client.Update(ctx, tunnel); err != nil {
			return fmt.Errorf("removing finalizer: %w", err)
		}
	}

	r.releaseForwarderPort(tunnel)
	r.notifyTunnelUpdate(ctx, tunnel, forwarderv1.UpdateType_DELETED)

	return client.IgnoreNotFound(r.Client.Delete(ctx, tunnel))
}

func (r *TunnelReconciler) deleteGatewayResources(ctx context.Context, tunnel *v1alpha1.Tunnel) error {
	resourceName := gatewayResourceName(tunnel.Name)
	objects := []client.Object{
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: tunnel.Namespace}},
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: tunnel.Namespace}},
	}

	for _, obj := range objects {
		if err := client.IgnoreNotFound(r.Client.Delete(ctx, obj)); err != nil {
			return fmt.Errorf("deleting %T: %w", obj, err)
		}
	}

	return nil
}

func (r *TunnelReconciler) resolveForwarderVIP(ctx context.Context) (string, error) {
	if r.Client == nil || r.ForwarderServiceKey.Name == "" {
		return "", fmt.Errorf("forwarder service not configured")
	}

	svc := &corev1.Service{}
	if err := r.Client.Get(ctx, r.ForwarderServiceKey, svc); err != nil {
		return "", fmt.Errorf("getting forwarder service %s: %w", r.ForwarderServiceKey, err)
	}

	for _, ingress := range svc.Status.LoadBalancer.Ingress {
		if ingress.IP != "" {
			return ingress.IP, nil
		}
	}

	return "", fmt.Errorf("forwarder service %s has no load balancer IP", r.ForwarderServiceKey)
}

func (r *TunnelReconciler) releaseForwarderPort(tunnel *v1alpha1.Tunnel) {
	if r.PortAllocator == nil {
		return
	}

	r.PortAllocator.Release(tunnelKey(tunnel))
	portAllocationsActive.Dec()
}

func (r *TunnelReconciler) notifyTunnelUpdate(ctx context.Context, tunnel *v1alpha1.Tunnel, updateType forwarderv1.UpdateType) {
	if r.ForwarderServer == nil || tunnel.Status.ForwarderPort <= 0 {
		return
	}

	gatewayAddress := net.JoinHostPort(tunnel.Status.GatewayPodName, strconv.Itoa(51820))
	if tunnel.Status.GatewayPodName != "" {
		pod := &corev1.Pod{}
		if err := r.Client.Get(ctx, client.ObjectKey{Namespace: tunnel.Namespace, Name: tunnel.Status.GatewayPodName}, pod); err == nil && pod.Status.PodIP != "" {
			gatewayAddress = net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(51820))
		}
	}

	forwarderPort := tunnel.Status.ForwarderPort
	tunnelName := tunnel.Name
	tunnelNamespace := tunnel.Namespace

	r.ForwarderServer.NotifyUpdate(&forwarderv1.TunnelUpdate{
		Type: &updateType,
		Tunnel: &forwarderv1.TunnelMapping{
			TunnelName:      &tunnelName,
			TunnelNamespace: &tunnelNamespace,
			ForwarderPort:   &forwarderPort,
			GatewayAddress:  &gatewayAddress,
		},
	})
}

func defaultFetchGatewayStatus(podIP string) (string, error) {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://%s:%d/status", podIP, gatewayStatusPort))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("gateway not ready: HTTP %d", resp.StatusCode)
	}

	var status struct {
		PublicKey string `json:"publicKey"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return "", fmt.Errorf("decoding gateway status: %w", err)
	}

	return status.PublicKey, nil
}

func tunnelKey(tunnel *v1alpha1.Tunnel) string {
	return tunnel.Namespace + "/" + tunnel.Name
}

func gatewayResourceName(tunnelName string) string {
	return "tunnel-gateway-" + tunnelName
}

func gatewayLabels(tunnelName string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by": "tunnel-operator",
		"tunnels.nais.io/tunnel":       tunnelName,
	}
}

func gatewayCapabilities(debug bool) *corev1.Capabilities {
	caps := &corev1.Capabilities{
		Drop: []corev1.Capability{"ALL"},
	}
	if debug {
		caps.Add = []corev1.Capability{"NET_RAW"}
	}
	return caps
}

func intstrPtr(i int32) *intstr.IntOrString {
	v := intstr.FromInt32(i)
	return &v
}

func isPodReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}

	return false
}

func (r *TunnelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("tunnel-controller").
		For(&v1alpha1.Tunnel{}).
		Owns(&corev1.Pod{}).
		Complete(r)
}
