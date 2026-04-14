package controller

import (
	"context"
	"fmt"
	"os"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/nais/tunnel-operator/api/v1alpha1"
)

const finalizerName = "tunnels.nais.io/cleanup"

type TunnelReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=nais.io,resources=tunnels,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=nais.io,resources=tunnels/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=nais.io,resources=tunnels/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;delete
//+kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;delete
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;delete
//+kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;delete

func (r *TunnelReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	tunnel := &v1alpha1.Tunnel{}
	if err := r.Get(ctx, req.NamespacedName, tunnel); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !tunnel.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, tunnel)
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
			if err := client.IgnoreNotFound(r.Delete(ctx, obj)); err != nil {
				return ctrl.Result{}, fmt.Errorf("deleting %T: %w", obj, err)
			}
		}

		if controllerutil.ContainsFinalizer(tunnel, finalizerName) {
			controllerutil.RemoveFinalizer(tunnel, finalizerName)
			if err := r.Update(ctx, tunnel); err != nil {
				return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
			}
		}

		if err := r.Delete(ctx, tunnel); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(tunnel, finalizerName) {
		controllerutil.AddFinalizer(tunnel, finalizerName)
		return ctrl.Result{}, r.Update(ctx, tunnel)
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
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sa, func() error {
		sa.Labels = labels
		return controllerutil.SetControllerReference(tunnel, sa, r.Scheme)
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling serviceaccount: %w", err)
	}

	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: tunnel.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, role, func() error {
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
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, roleBinding, func() error {
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
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, pod, func() error {
		pod.Labels = labels
		pod.Spec = corev1.PodSpec{
			ServiceAccountName:    saName,
			RestartPolicy:         corev1.RestartPolicyNever,
			ActiveDeadlineSeconds: &deadlineSeconds,
			Containers: []corev1.Container{{
				Name:  "gateway",
				Image: gatewayImage,
				Env: []corev1.EnvVar{
					{Name: "TUNNEL_PEER_PUBLIC_KEY", Value: tunnel.Spec.ClientPublicKey},
					{Name: "TUNNEL_TARGET_HOST", Value: tunnel.Spec.Target.Host},
					{Name: "TUNNEL_TARGET_PORT", Value: strconv.Itoa(int(tunnel.Spec.Target.Port))},
					{Name: "STUN_SERVERS", Value: "stun.cloudflare.com:3478,stun.l.google.com:19302"},
					{Name: "TUNNEL_NAME", Value: tunnel.Name},
					{Name: "TUNNEL_NAMESPACE", Value: tunnel.Namespace},
				},
			}},
		}
		return controllerutil.SetControllerReference(tunnel, pod, r.Scheme)
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling pod: %w", err)
	}

	networkPolicy := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: tunnel.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, networkPolicy, func() error {
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
						Protocol: protocolPtr(corev1.ProtocolTCP),
					}},
				},
				{
					Ports: []networkingv1.NetworkPolicyPort{{
						Protocol: protocolPtr(corev1.ProtocolUDP),
					}},
				},
			},
		}
		return controllerutil.SetControllerReference(tunnel, networkPolicy, r.Scheme)
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconciling networkpolicy: %w", err)
	}

	updated := false
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
	if updated {
		if err := r.Status().Update(ctx, tunnel); err != nil {
			return ctrl.Result{}, fmt.Errorf("updating tunnel status: %w", err)
		}
	}

	logger.Info("reconciled tunnel", "tunnel", req.NamespacedName, "pod", resourceName)

	return ctrl.Result{}, nil
}

func (r *TunnelReconciler) handleDeletion(ctx context.Context, tunnel *v1alpha1.Tunnel) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(tunnel, finalizerName) {
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
		if err := client.IgnoreNotFound(r.Delete(ctx, obj)); err != nil {
			return ctrl.Result{}, fmt.Errorf("deleting %T: %w", obj, err)
		}
	}

	controllerutil.RemoveFinalizer(tunnel, finalizerName)
	if err := r.Update(ctx, tunnel); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}

	return ctrl.Result{}, nil
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

func intstrPtr(i int32) *intstr.IntOrString {
	v := intstr.FromInt32(i)
	return &v
}

func protocolPtr(p corev1.Protocol) *corev1.Protocol {
	return &p
}

func (r *TunnelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Tunnel{}).
		Complete(r)
}
