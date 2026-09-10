package main

import (
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// serveConfig is the fully parsed configuration of a `kubeswap serve`
// invocation. The render functions produce deterministic objects from it.
type serveConfig struct {
	Model       string // original model ID
	Sanitized   string // DNS-safe model ID
	Namespace   string
	Image       string
	Args        []string // container args (everything after --)
	Port        int32    // container port the backend listens on
	HealthPath  string
	Env         []envVar
	GPUs        map[string]string // resource name -> quantity (limits)
	NodeSel     map[string]string
	Tolerations []toleration
	Volumes     []volumeSpec
	ExtraLabels map[string]string

	// PVC defaults, used when a --volume pvc:<name> does not exist yet.
	PVCSize       string
	PVCClass      string
	PVCAccessMode string

	GraceSeconds int64
	Strict       bool
}

// pvcNames returns the names of PVCs referenced by the config.
func (c *serveConfig) pvcNames() []string {
	var out []string
	for _, v := range c.Volumes {
		if v.Kind == volPVC {
			out = append(out, v.Name)
		}
	}
	return out
}

// renderDeployment builds the model's Deployment.
func (c *serveConfig) renderDeployment() (*appsv1.Deployment, error) {
	depLabels := map[string]string{labelAppName: appNameValue}
	for k, v := range managedLabels(c.Sanitized) {
		depLabels[k] = v
	}
	for k, v := range c.ExtraLabels {
		depLabels[k] = v
	}

	podTemplateLabels := podLabels(c.Sanitized)
	for k, v := range c.ExtraLabels {
		podTemplateLabels[k] = v
	}

	container := corev1.Container{
		Name:      containerName,
		Image:     c.Image,
		Args:      c.Args,
		Ports:     []corev1.ContainerPort{{ContainerPort: c.Port}},
		Env:       c.renderEnv(),
		Resources: c.renderResources(),
		ReadinessProbe: &corev1.Probe{
			ProbeHandler:        httpProbe(c.HealthPath, c.Port),
			InitialDelaySeconds: 2,
			PeriodSeconds:       5,
			TimeoutSeconds:      5,
			FailureThreshold:    3,
		},
		LivenessProbe: &corev1.Probe{
			ProbeHandler:        httpProbe(c.HealthPath, c.Port),
			InitialDelaySeconds: 30,
			PeriodSeconds:       15,
			TimeoutSeconds:      5,
			FailureThreshold:    3,
		},
	}

	volumes, mounts, err := c.renderVolumes()
	if err != nil {
		return nil, err
	}
	container.VolumeMounts = mounts

	annotations := map[string]string{annotationModelID: c.Model}
	recreate := appsv1.RecreateDeploymentStrategyType
	replicas := int32(1)

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        deploymentName(c.Sanitized),
			Namespace:   c.Namespace,
			Labels:      depLabels,
			Annotations: annotations,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Strategy: appsv1.DeploymentStrategy{Type: recreate},
			Selector: &metav1.LabelSelector{MatchLabels: managedLabels(c.Sanitized)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      podTemplateLabels,
					Annotations: map[string]string{annotationModelID: c.Model},
				},
				Spec: corev1.PodSpec{
					Containers:                    []corev1.Container{container},
					Volumes:                       volumes,
					NodeSelector:                  c.NodeSel,
					Tolerations:                   c.renderTolerations(),
					TerminationGracePeriodSeconds: &c.GraceSeconds,
					RestartPolicy:                 corev1.RestartPolicyAlways,
				},
			},
		},
	}, nil
}

func (c *serveConfig) renderEnv() []corev1.EnvVar {
	out := make([]corev1.EnvVar, 0, len(c.Env))
	for _, e := range c.Env {
		out = append(out, corev1.EnvVar{Name: e.Key, Value: e.Value})
	}
	return out
}

func (c *serveConfig) renderResources() corev1.ResourceRequirements {
	if len(c.GPUs) == 0 {
		return corev1.ResourceRequirements{}
	}
	limits := corev1.ResourceList{}
	for _, k := range sortedKeys(c.GPUs) {
		qty, err := resource.ParseQuantity(c.GPUs[k])
		if err != nil {
			// An unparseable quantity is surfaced at parse time already;
			// fall back to a string quantity so rendering cannot fail.
			qty = resource.MustParse("0")
		}
		limits[corev1.ResourceName(k)] = qty
	}
	return corev1.ResourceRequirements{Limits: limits}
}

func (c *serveConfig) renderTolerations() []corev1.Toleration {
	out := make([]corev1.Toleration, 0, len(c.Tolerations))
	for _, t := range c.Tolerations {
		out = append(out, corev1.Toleration{
			Key:      t.Key,
			Operator: corev1.TolerationOperator(t.Operator),
			Value:    t.Value,
			Effect:   corev1.TaintEffect(t.Effect),
		})
	}
	return out
}

// renderVolumes converts --volume entries into pod volumes + mounts.
func (c *serveConfig) renderVolumes() (volumes []corev1.Volume, mounts []corev1.VolumeMount, err error) {
	for i, v := range c.Volumes {
		name := fmt.Sprintf("vol%d", i)
		switch v.Kind {
		case volPVC:
			volumes = append(volumes, corev1.Volume{
				Name: name,
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: v.Name,
						ReadOnly:  v.ReadOnly,
					},
				},
			})
		case volEmptyDir:
			volumes = append(volumes, corev1.Volume{
				Name:         name,
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			})
		case volHostPath:
			volumes = append(volumes, corev1.Volume{
				Name: name,
				VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{Path: v.Name},
				},
			})
		default:
			return nil, nil, fmt.Errorf("unknown volume kind %q", v.Kind)
		}
		mounts = append(mounts, corev1.VolumeMount{
			Name:      name,
			MountPath: v.Path,
			ReadOnly:  v.ReadOnly,
		})
	}
	return volumes, mounts, nil
}

func httpProbe(path string, port int32) corev1.ProbeHandler {
	return corev1.ProbeHandler{
		HTTPGet: &corev1.HTTPGetAction{Path: path, Port: intstr.FromInt32(port)},
	}
}

// renderService builds the model's ClusterIP Service.
func (c *serveConfig) renderService() *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        serviceName(c.Sanitized),
			Namespace:   c.Namespace,
			Labels:      managedLabels(c.Sanitized),
			Annotations: map[string]string{annotationModelID: c.Model},
		},
		Spec: corev1.ServiceSpec{
			Selector: managedLabels(c.Sanitized),
			Ports: []corev1.ServicePort{{
				Name:       "http",
				Port:       c.Port,
				TargetPort: intstr.FromInt32(c.Port),
			}},
		},
	}
}

// renderPVC builds a PVC for a missing --volume pvc:<name>.
func (c *serveConfig) renderPVC(name string) (*corev1.PersistentVolumeClaim, error) {
	size, err := resource.ParseQuantity(c.PVCSize)
	if err != nil {
		return nil, fmt.Errorf("invalid --pvc-size %q: %v", c.PVCSize, err)
	}
	mode := corev1.ReadWriteOnce
	if strings.EqualFold(c.PVCAccessMode, "rwx") {
		mode = corev1.ReadWriteMany
	}
	accessModes := []corev1.PersistentVolumeAccessMode{mode}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   c.Namespace,
			Labels:      managedLabels(c.Sanitized),
			Annotations: map[string]string{annotationModelID: c.Model},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      accessModes,
			Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: size}},
			StorageClassName: strPtr(c.PVCClass),
		},
	}
	return pvc, nil
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// deploymentSpecMatches reports whether an existing deployment's container
// spec matches the desired one (used by --strict adoption).
func deploymentSpecMatches(have *appsv1.Deployment, want *appsv1.Deployment) bool {
	if have == nil || want == nil {
		return false
	}
	hc, wc := deploymentContainer(have), deploymentContainer(want)
	if hc == nil || wc == nil {
		return hc == wc
	}
	if hc.Image != wc.Image {
		return false
	}
	if !stringSlicesEqual(hc.Args, wc.Args) {
		return false
	}
	if !envsEqual(hc.Env, wc.Env) {
		return false
	}
	return resourcesEqual(hc.Resources, wc.Resources)
}

func deploymentContainer(dep *appsv1.Deployment) *corev1.Container {
	containers := dep.Spec.Template.Spec.Containers
	for i := range containers {
		if containers[i].Name == containerName {
			return &containers[i]
		}
	}
	if len(containers) > 0 {
		return &containers[0]
	}
	return nil
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func envsEqual(a, b []corev1.EnvVar) bool {
	if len(a) != len(b) {
		return false
	}
	byKey := func(vars []corev1.EnvVar) map[string]string {
		m := make(map[string]string, len(vars))
		for _, v := range vars {
			m[v.Name] = v.Value
		}
		return m
	}
	ma, mb := byKey(a), byKey(b)
	for k, v := range ma {
		if mb[k] != v {
			return false
		}
	}
	return true
}

func resourcesEqual(a, b corev1.ResourceRequirements) bool {
	return resourceListEqual(a.Limits, b.Limits) && resourceListEqual(a.Requests, b.Requests)
}

func resourceListEqual(a, b corev1.ResourceList) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if !b[k].Equal(v) {
			return false
		}
	}
	return true
}
