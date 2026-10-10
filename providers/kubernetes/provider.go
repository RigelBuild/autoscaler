package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/urfave/cli/v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"

	"go.woodpecker-ci.org/autoscaler/config"
	"go.woodpecker-ci.org/autoscaler/engine"
	"go.woodpecker-ci.org/autoscaler/engine/types"
	"go.woodpecker-ci.org/woodpecker/v3/woodpecker-go/woodpecker"
)

const (
	agentNameAnnotation       = "wp.autoscaler/agent-name"
	woodpeckerAgentSecret     = "WOODPECKER_AGENT_SECRET"
	woodpeckerAgentSecretFile = "WOODPECKER_AGENT_SECRET_FILE"
	maxPodNameLength          = 253
)

var invalidPodNameCharacters = regexp.MustCompile(`[^a-z0-9-]+`)

type provider struct {
	client      kubernetes.Interface
	namespace   string
	config      *config.Config
	podTemplate corev1.Pod
}

func New(_ context.Context, c *cli.Command, config *config.Config) (types.Provider, error) {
	namespace := c.String("kubernetes-namespace")
	if namespace == "" {
		return nil, errors.New("kubernetes namespace is required")
	}

	podTemplate, err := loadPodTemplate(c.String("kubernetes-pod-template-file"))
	if err != nil {
		return nil, err
	}

	restConfig, err := clientcmd.BuildConfigFromFlags("", c.String("kubernetes-kubeconfig"))
	if err != nil {
		return nil, fmt.Errorf("build kubernetes client config: %w", err)
	}
	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("create kubernetes client: %w", err)
	}

	return &provider{
		client:      client,
		namespace:   namespace,
		config:      config,
		podTemplate: podTemplate,
	}, nil
}

func loadPodTemplate(path string) (corev1.Pod, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return corev1.Pod{}, fmt.Errorf("read kubernetes pod template: %w", err)
	}

	var podTemplate corev1.Pod
	if err := yaml.UnmarshalStrict(data, &podTemplate); err != nil {
		return corev1.Pod{}, fmt.Errorf("decode kubernetes pod template: %w", err)
	}
	if len(podTemplate.Spec.Containers) == 0 {
		return corev1.Pod{}, errors.New("kubernetes pod template has no containers")
	}
	for key := range podTemplate.Labels {
		if strings.HasPrefix(key, engine.LabelPrefix) {
			return corev1.Pod{}, fmt.Errorf("kubernetes pod template: reserved label prefix %q", engine.LabelPrefix)
		}
	}
	for key := range podTemplate.Annotations {
		if strings.HasPrefix(key, engine.LabelPrefix) {
			return corev1.Pod{}, fmt.Errorf("kubernetes pod template: reserved annotation prefix %q", engine.LabelPrefix)
		}
	}
	return podTemplate, nil
}

func (p *provider) DeployAgent(ctx context.Context, agent *woodpecker.Agent) error {
	name := podName(agent.Name, agent.ID)
	pod := p.podTemplate.DeepCopy()
	pod.Name = name
	pod.Namespace = p.namespace
	if pod.Labels == nil {
		pod.Labels = make(map[string]string)
	}
	pod.Labels[engine.LabelPool] = p.config.PoolID
	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string)
	}
	pod.Annotations[agentNameAnnotation] = agent.Name
	if pod.Spec.RestartPolicy == "" {
		pod.Spec.RestartPolicy = corev1.RestartPolicyAlways
	}

	container := &pod.Spec.Containers[0]
	if container.Image == "" {
		container.Image = p.config.Image
	}
	container.Env = withoutAgentSecrets(container.Env)
	container.Env = append(container.Env,
		corev1.EnvVar{Name: "WOODPECKER_SERVER", Value: p.config.GRPCAddress},
	)
	if p.config.GRPCSecure {
		container.Env = append(container.Env, corev1.EnvVar{Name: "WOODPECKER_GRPC_SECURE", Value: "true"})
	}
	container.Env = append(container.Env, corev1.EnvVar{
		Name:  "WOODPECKER_MAX_WORKFLOWS",
		Value: strconv.Itoa(p.config.WorkflowsPerAgent),
	})
	// Sorted so every pod of a pool has one exact env, which admission allowlists.
	for _, key := range slices.Sorted(maps.Keys(p.config.Environment)) {
		if key == woodpeckerAgentSecret || key == woodpeckerAgentSecretFile {
			continue
		}
		container.Env = append(container.Env, corev1.EnvVar{Name: key, Value: p.config.Environment[key]})
	}
	container.Env = append(container.Env,
		corev1.EnvVar{Name: "WOODPECKER_AGENT_LABELS", Value: sortedAgentLabels(p.config.ExtraAgentLabels)},
		corev1.EnvVar{
			Name: woodpeckerAgentSecret,
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: name},
				Key:                  woodpeckerAgentSecret,
			}},
		},
	)

	createdPod, err := p.client.CoreV1().Pods(p.namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create Kubernetes agent pod: %w", err)
	}

	_, err = p.client.CoreV1().Secrets(p.namespace).Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: p.namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "v1",
				Kind:       "Pod",
				Name:       createdPod.Name,
				UID:        createdPod.UID,
				Controller: ptr.To(false),
			}},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{woodpeckerAgentSecret: []byte(agent.Token)},
	}, metav1.CreateOptions{})
	if err != nil {
		if cleanupErr := p.client.CoreV1().Pods(p.namespace).Delete(ctx, createdPod.Name, metav1.DeleteOptions{}); cleanupErr != nil && !apierrors.IsNotFound(cleanupErr) {
			return errors.Join(fmt.Errorf("create Kubernetes agent secret: %w", err), fmt.Errorf("delete pod after Secret creation failure: %w", cleanupErr))
		}
		return fmt.Errorf("create Kubernetes agent secret: %w", err)
	}
	return nil
}

func (p *provider) RemoveAgent(ctx context.Context, agent *woodpecker.Agent) error {
	if agent.ID == 0 {
		pods, err := p.client.CoreV1().Pods(p.namespace).List(ctx, metav1.ListOptions{
			LabelSelector: engine.LabelPool + "=" + p.config.PoolID,
		})
		if err != nil {
			return fmt.Errorf("list Kubernetes agent pods: %w", err)
		}
		var errs []error
		for i := range pods.Items {
			pod := &pods.Items[i]
			if pod.Annotations[agentNameAnnotation] != agent.Name {
				continue
			}
			errs = append(errs, p.deleteAgentPod(ctx, pod.Name))
		}
		return errors.Join(errs...)
	}
	return p.deleteAgentPod(ctx, podName(agent.Name, agent.ID))
}

func (p *provider) deleteAgentPod(ctx context.Context, name string) error {
	if err := p.client.CoreV1().Pods(p.namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete Kubernetes agent pod: %w", err)
	}
	return nil
}

func (p *provider) ListDeployedAgentNames(ctx context.Context) ([]string, error) {
	pods, err := p.client.CoreV1().Pods(p.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: engine.LabelPool + "=" + p.config.PoolID,
	})
	if err != nil {
		return nil, fmt.Errorf("list Kubernetes agent pods: %w", err)
	}

	names := make([]string, 0, len(pods.Items))
	for i := range pods.Items {
		if name, ok := pods.Items[i].Annotations[agentNameAnnotation]; ok {
			names = append(names, name)
		}
	}
	return names, nil
}

func (p *provider) BillingModel() types.BillingModel {
	return types.BillingPerSecond
}

func podName(agentName string, agentID int64) string {
	name := strings.ToLower(agentName)
	name = invalidPodNameCharacters.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-")
	if name == "" {
		name = "agent"
	}

	suffix := "-" + strconv.FormatInt(agentID, 10)
	maxNameLength := maxPodNameLength - len(suffix)
	if len(name) > maxNameLength {
		name = strings.TrimRight(name[:maxNameLength], "-")
	}
	return name + suffix
}

func sortedAgentLabels(labels map[string]string) string {
	out := make([]string, 0, len(labels))
	for _, key := range slices.Sorted(maps.Keys(labels)) {
		out = append(out, key+"="+labels[key])
	}
	return strings.Join(out, ",")
}

func withoutAgentSecrets(env []corev1.EnvVar) []corev1.EnvVar {
	result := env[:0]
	for _, variable := range env {
		if variable.Name != woodpeckerAgentSecret && variable.Name != woodpeckerAgentSecretFile {
			result = append(result, variable)
		}
	}
	return result
}
