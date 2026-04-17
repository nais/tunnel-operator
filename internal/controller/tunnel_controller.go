package controller

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"

	v1alpha1 "github.com/nais/tunnel-operator/api/v1alpha1"
	operatorgrpc "github.com/nais/tunnel-operator/internal/grpc"
	forwarderv1 "github.com/nais/tunnel-operator/pkg/forwarder/proto/forwarder/v1"
	"github.com/nais/tunnel-operator/pkg/portalloc"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"
)

const finalizerName = "tunnels.nais.io/cleanup"

// ClusterProvider returns a cluster.Cluster for a given cluster name.
// mcmanager.Manager satisfies this interface.
type ClusterProvider interface {
	GetCluster(ctx context.Context, clusterName multicluster.ClusterName) (cluster.Cluster, error)
}

type TunnelReconciler struct {
	ClusterProvider ClusterProvider
	Scheme          *runtime.Scheme
	PortAllocator   *portalloc.PortAllocator
	ForwarderServer *operatorgrpc.ForwarderServer
	LBVIP           string
}

//+kubebuilder:rbac:groups=nais.io,resources=tunnels,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=nais.io,resources=tunnels/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=nais.io,resources=tunnels/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;delete
//+kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;delete
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;delete
//+kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;delete

func (r *TunnelReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("cluster", req.ClusterName)

	cl, err := r.ClusterProvider.GetCluster(ctx, req.ClusterName)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("getting cluster %s: %w", req.ClusterName, err)
	}
	clusterClient := cl.GetClient()

	tunnel := &v1alpha1.Tunnel{}
	if err := clusterClient.Get(ctx, req.NamespacedName, tunnel); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !tunnel.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, clusterClient, tunnel)
	}

	if tunnel.Status.Phase == v1alpha1.TunnelPhaseTerminated {
		logger.Info("cleaning up terminated tunnel", "tunnel", req.NamespacedName)

		resourceName := gatewayResourceName(tunnel.Name)
		objects := []client.Object{
			&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: tunnel.Namespace}},
			&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: tunnel.Namespace}},
			&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: tunnel.Namespace}},
			&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: tunnel.Namespace}},
			&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: tunnel.Namespace}},
		}
		for _, obj := range objects {
			if err := client.IgnoreNotFound(clusterClient.Delete(ctx, obj)); err != nil {
				return ctrl.Result{}, fmt.Errorf("deleting %T: %w", obj, err)
			}
		}

		if controllerutil.ContainsFinalizer(tunnel, finalizerName) {
			controllerutil.RemoveFinalizer(tunnel, finalizerName)
			if err := clusterClient.Update(ctx, tunnel); err != nil {
				return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
			}
		}

		r.releaseForwarderPort(tunnel)
		r.notifyTunnelUpdate(ctx, clusterClient, tunnel, forwarderv1.UpdateType_DELETED)

		if err := clusterClient.Delete(ctx, tunnel); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(tunnel, finalizerName) {
		controllerutil.AddFinalizer(tunnel, finalizerName)
		return ctrl.Result{}, clusterClient.Update(ctx, tunnel)
	}

	resourceName := gatewayResourceName(tunnel.Name)
	labels := gatewayLabels(tunnel.Name)
	saName := resourceName
	gatewayImage := os.Getenv("GATEWAY_IMAGE")
	if gatewayImage == "" {
		gatewayImage = "ghcr.io/nais/tunnel-operator/gateway:latest"
	}

	deadlineSeconds := int64(3600)
	if tunnel.Spec.ActiveDeadlineSeconds != nil {
		deadlineSeconds = *tunnel.Spec.ActiveDeadlineSeconds
	}

	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: saName, Namespace: tunnel.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, clusterClient, sa, func() error {
		sa.Labels = labels
		return controllerutil.SetControllerReference(tunnel, sa, r.Scheme)
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling serviceaccount: %w", err)
	}

	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: tunnel.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, clusterClient, role, func() error {
		role.Labels = labels
		role.Rules = []rbacv1.PolicyRule{
			{
				APIGroups: []string{"nais.io"},
				Resources: []string{"tunnels"},
				Verbs:     []string{"get"},
			},
			{
				APIGroups: []string{"nais.io"},
				Resources: []string{"tunnels/status"},
				Verbs:     []string{"get", "patch"},
			},
		}
		return controllerutil.SetControllerReference(tunnel, role, r.Scheme)
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling role: %w", err)
	}

	roleBinding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: tunnel.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, clusterClient, roleBinding, func() error {
		roleBinding.Labels = labels
		roleBinding.RoleRef = rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     resourceName,
		}
		roleBinding.Subjects = []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      saName,
			Namespace: tunnel.Namespace,
		}}
		return controllerutil.SetControllerReference(tunnel, roleBinding, r.Scheme)
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling rolebinding: %w", err)
	}

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: tunnel.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, clusterClient, pod, func() error {
		pod.Labels = labels
		pod.Spec = corev1.PodSpec{
			ServiceAccountName:    saName,
			RestartPolicy:         corev1.RestartPolicyNever,
			ActiveDeadlineSeconds: &deadlineSeconds,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: new(true),
				SeccompProfile: &corev1.SeccompProfile{
					Type: corev1.SeccompProfileTypeRuntimeDefault,
				},
			},
			Containers: []corev1.Container{{
				Name:  "gateway",
				Image: gatewayImage,
				Env: []corev1.EnvVar{
					{Name: "TUNNEL_PEER_PUBLIC_KEY", Value: tunnel.Spec.ClientPublicKey},
					{Name: "TUNNEL_TARGET_HOST", Value: tunnel.Spec.Target.Host},
					{Name: "TUNNEL_TARGET_PORT", Value: strconv.Itoa(int(tunnel.Spec.Target.Port))},
					{Name: "TUNNEL_NAME", Value: tunnel.Name},
					{Name: "TUNNEL_NAMESPACE", Value: tunnel.Namespace},
				},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: new(false),
					RunAsNonRoot:             new(true),
					SeccompProfile: &corev1.SeccompProfile{
						Type: corev1.SeccompProfileTypeRuntimeDefault,
					},
					Capabilities: &corev1.Capabilities{
						Drop: []corev1.Capability{"ALL"},
					},
				},
			}},
		}
		return controllerutil.SetControllerReference(tunnel, pod, r.Scheme)
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling pod: %w", err)
	}

	networkPolicy := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: tunnel.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, clusterClient, networkPolicy, func() error {
		networkPolicy.Labels = labels
		networkPolicy.Spec = networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{"tunnels.nais.io/tunnel": tunnel.Name},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					To: []networkingv1.NetworkPolicyPeer{{
						IPBlock: &networkingv1.IPBlock{CIDR: tunnel.Spec.Target.ResolvedIP + "/32"},
					}},
					Ports: []networkingv1.NetworkPolicyPort{{
						Port:     intstrPtr(tunnel.Spec.Target.Port),
						Protocol: ptr.To(corev1.ProtocolTCP),
					}},
				},
				{
					Ports: []networkingv1.NetworkPolicyPort{{
						Protocol: ptr.To(corev1.ProtocolUDP),
					}},
				},
			},
		}
		return controllerutil.SetControllerReference(tunnel, networkPolicy, r.Scheme)
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling networkpolicy: %w", err)
	}

	updated := false
	updateType := forwarderv1.UpdateType_MODIFIED
	if tunnel.Status.ForwarderPort == 0 && r.PortAllocator != nil {
		allocatedPort, err := r.PortAllocator.Allocate(tunnelKey(tunnel))
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("allocating forwarder port: %w", err)
		}
		tunnel.Status.ForwarderPort = allocatedPort
		updated = true
		updateType = forwarderv1.UpdateType_ADDED
	}
	if tunnel.Status.Phase != v1alpha1.TunnelPhaseProvisioning {
		tunnel.Status.Phase = v1alpha1.TunnelPhaseProvisioning
		updated = true
	}
	if tunnel.Status.Message != "Gateway pod starting" {
		tunnel.Status.Message = "Gateway pod starting"
		updated = true
	}
	if tunnel.Status.GatewayPodName != resourceName {
		tunnel.Status.GatewayPodName = resourceName
		updated = true
	}
	if isPodReady(pod) {
		forwarderEndpoint := net.JoinHostPort(r.LBVIP, strconv.Itoa(int(tunnel.Status.ForwarderPort)))
		if tunnel.Status.ForwarderEndpoint != forwarderEndpoint {
			tunnel.Status.ForwarderEndpoint = forwarderEndpoint
			updated = true
		}
	}
	if updated {
		if err := clusterClient.Status().Update(ctx, tunnel); err != nil {
			return ctrl.Result{}, fmt.Errorf("updating tunnel status: %w", err)
		}
		r.notifyTunnelUpdate(ctx, clusterClient, tunnel, updateType)
	}

	logger.Info("reconciled tunnel", "tunnel", req.NamespacedName, "pod", resourceName)

	return ctrl.Result{}, nil
}

func (r *TunnelReconciler) handleDeletion(ctx context.Context, clusterClient client.Client, tunnel *v1alpha1.Tunnel) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(tunnel, finalizerName) {
		r.releaseForwarderPort(tunnel)
		return ctrl.Result{}, nil
	}

	resourceName := gatewayResourceName(tunnel.Name)
	objects := []client.Object{
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: tunnel.Namespace}},
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: tunnel.Namespace}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: tunnel.Namespace}},
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: tunnel.Namespace}},
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: tunnel.Namespace}},
	}

	for _, obj := range objects {
		if err := client.IgnoreNotFound(clusterClient.Delete(ctx, obj)); err != nil {
			return ctrl.Result{}, fmt.Errorf("deleting %T: %w", obj, err)
		}
	}

	controllerutil.RemoveFinalizer(tunnel, finalizerName)
	if err := clusterClient.Update(ctx, tunnel); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}

	r.releaseForwarderPort(tunnel)
	r.notifyTunnelUpdate(ctx, clusterClient, tunnel, forwarderv1.UpdateType_DELETED)

	return ctrl.Result{}, nil
}

func (r *TunnelReconciler) releaseForwarderPort(tunnel *v1alpha1.Tunnel) {
	if r.PortAllocator == nil {
		return
	}

	r.PortAllocator.Release(tunnelKey(tunnel))
}

func (r *TunnelReconciler) notifyTunnelUpdate(ctx context.Context, clusterClient client.Client, tunnel *v1alpha1.Tunnel, updateType forwarderv1.UpdateType) {
	if r.ForwarderServer == nil || tunnel.Status.ForwarderPort <= 0 {
		return
	}

	gatewayAddress := net.JoinHostPort(tunnel.Status.GatewayPodName, strconv.Itoa(51820))
	if tunnel.Status.GatewayPodName != "" {
		pod := &corev1.Pod{}
		if err := clusterClient.Get(ctx, client.ObjectKey{Namespace: tunnel.Namespace, Name: tunnel.Status.GatewayPodName}, pod); err == nil && pod.Status.PodIP != "" {
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
		"apiserver-access":             "enabled",
	}
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

func (r *TunnelReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	r.ClusterProvider = mgr
	return mcbuilder.ControllerManagedBy(mgr).
		Named("tunnel-controller").
		For(&v1alpha1.Tunnel{}).
		Complete(r)
}
