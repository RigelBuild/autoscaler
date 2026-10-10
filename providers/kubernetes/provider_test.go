package kubernetes

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"go.woodpecker-ci.org/autoscaler/config"
	"go.woodpecker-ci.org/autoscaler/engine"
	"go.woodpecker-ci.org/woodpecker/v3/woodpecker-go/woodpecker"
)

const (
	testNamespace = "woodpecker-agents"
	testPoolID    = "pool-7"
)

func TestDeployAgent(t *testing.T) {
	for _, tt := range []struct {
		name          string
		templateImage string
		wantImage     string
		grpcSecure    bool
	}{
		{name: "keep template image", templateImage: "custom-agent:v1", wantImage: "custom-agent:v1", grpcSecure: true},
		{name: "use configured image fallback", wantImage: "configured-agent:v2"},
		{name: "omit grpc secure when disabled", templateImage: "custom-agent:v1", wantImage: "custom-agent:v1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.GRPCSecure = tt.grpcSecure
			cfg.Environment[woodpeckerAgentSecret] = "config-override"
			cfg.Environment[woodpeckerAgentSecretFile] = "config-file-override"
			p, client := newTestProvider(tt.templateImage, cfg)
			client.PrependReactor("create", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
				createAction, ok := action.(ktesting.CreateAction)
				if !ok {
					return true, nil, errors.New("pod create action has unexpected type")
				}
				pod, ok := createAction.GetObject().(*corev1.Pod)
				if !ok {
					return true, nil, errors.New("pod create object has unexpected type")
				}
				pod.UID = "fixed-pod-uid"
				return false, nil, nil
			})
			agent := &woodpecker.Agent{ID: 42, Name: "pool-7-agent-aBcXYZ", Token: "secret-token"}
			require.NoError(t, p.DeployAgent(t.Context(), agent))
			assert.Equal(t, []string{"create:pods", "create:secrets"}, actionSummaries(client.Actions()))

			pod, err := client.CoreV1().Pods(testNamespace).Get(t.Context(), "pool-7-agent-abcxyz-42", metav1.GetOptions{})
			require.NoError(t, err)
			assert.Equal(t, agent.Name, pod.Annotations[agentNameAnnotation])
			assert.Equal(t, testPoolID, pod.Labels[engine.LabelPool])
			assert.Equal(t, "custom", pod.Annotations["template-annotation"])
			assert.Equal(t, "woodpecker", pod.Spec.ServiceAccountName)
			assert.Equal(t, corev1.RestartPolicyAlways, pod.Spec.RestartPolicy)
			require.Len(t, pod.Spec.Containers, 1)
			container := pod.Spec.Containers[0]
			assert.Equal(t, tt.wantImage, container.Image)
			assert.Equal(t, "team=ci", envValue(container.Env, "WOODPECKER_AGENT_LABELS"))
			assert.Equal(t, "kept", envValue(container.Env, "TEMPLATE_ENV"))
			assert.Equal(t, cfg.GRPCAddress, envValue(container.Env, "WOODPECKER_SERVER"))
			assert.Equal(t, "3", envValue(container.Env, "WOODPECKER_MAX_WORKFLOWS"))
			assert.Equal(t, "value", envValue(container.Env, "CUSTOM_ENV"))
			assert.Empty(t, envValue(container.Env, woodpeckerAgentSecretFile))
			if tt.grpcSecure {
				assert.Equal(t, "true", envValue(container.Env, "WOODPECKER_GRPC_SECURE"))
			} else {
				assert.Empty(t, envValue(container.Env, "WOODPECKER_GRPC_SECURE"))
			}

			var secretEnv *corev1.EnvVar
			for i := range container.Env {
				if container.Env[i].Name == woodpeckerAgentSecret {
					secretEnv = &container.Env[i]
					break
				}
			}
			require.NotNil(t, secretEnv)
			assert.Empty(t, secretEnv.Value)
			require.NotNil(t, secretEnv.ValueFrom)
			require.NotNil(t, secretEnv.ValueFrom.SecretKeyRef)
			assert.Equal(t, "pool-7-agent-abcxyz-42", secretEnv.ValueFrom.SecretKeyRef.Name)
			assert.Equal(t, woodpeckerAgentSecret, secretEnv.ValueFrom.SecretKeyRef.Key)
			secretAction, ok := client.Actions()[1].(ktesting.CreateAction)
			require.True(t, ok)
			secret, ok := secretAction.GetObject().(*corev1.Secret)
			require.True(t, ok)
			assert.Equal(t, corev1.SecretTypeOpaque, secret.Type)
			assert.Equal(t, []byte(agent.Token), secret.Data[woodpeckerAgentSecret])
			assert.Empty(t, secret.Annotations)
			require.Len(t, secret.OwnerReferences, 1)
			assert.Equal(t, types.UID("fixed-pod-uid"), pod.UID)
			assert.Equal(t, pod.UID, secret.OwnerReferences[0].UID)
			assert.Equal(t, pod.Name, secret.OwnerReferences[0].Name)
			require.NotNil(t, secret.OwnerReferences[0].Controller)
			assert.False(t, *secret.OwnerReferences[0].Controller)
		})
	}
}

func TestDeployAgentEnvIsDeterministic(t *testing.T) {
	cfg := testConfig()
	cfg.Environment = map[string]string{"B_ENV": "2", "A_ENV": "1", "C_ENV": "3"}
	cfg.ExtraAgentLabels = map[string]string{"os": "linux", "cloud": "azure", "lifecycle": "spot"}
	p, client := newTestProvider("custom-agent:v1", cfg)
	for id := int64(1); id <= 20; id++ {
		require.NoError(t, p.DeployAgent(t.Context(), &woodpecker.Agent{ID: id, Name: "pool-7-agent-x", Token: "t"}))
		pod, err := client.CoreV1().Pods(testNamespace).Get(t.Context(), podName("pool-7-agent-x", id), metav1.GetOptions{})
		require.NoError(t, err)
		names := make([]string, 0, len(pod.Spec.Containers[0].Env))
		for _, e := range pod.Spec.Containers[0].Env {
			names = append(names, e.Name)
		}
		assert.Equal(t, []string{
			"TEMPLATE_ENV", "WOODPECKER_SERVER", "WOODPECKER_MAX_WORKFLOWS",
			"A_ENV", "B_ENV", "C_ENV", "WOODPECKER_AGENT_LABELS", woodpeckerAgentSecret,
		}, names)
		assert.Equal(t, "cloud=azure,lifecycle=spot,os=linux", envValue(pod.Spec.Containers[0].Env, "WOODPECKER_AGENT_LABELS"))
	}
}

func TestDeployAgentPodCreateFailureDoesNotCreateSecret(t *testing.T) {
	p, client := newTestProvider("", testConfig())
	var secretReadsOrDeletes int
	client.PrependReactor("get", "secrets", func(ktesting.Action) (bool, runtime.Object, error) {
		secretReadsOrDeletes++
		return true, nil, errors.New("Secret get must not be called")
	})
	client.PrependReactor("delete", "secrets", func(ktesting.Action) (bool, runtime.Object, error) {
		secretReadsOrDeletes++
		return true, nil, errors.New("Secret delete must not be called")
	})
	client.PrependReactor("create", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("pod create rejected")
	})

	err := p.DeployAgent(t.Context(), &woodpecker.Agent{ID: 9, Name: "pool-7-agent-ABC", Token: "token"})
	require.Error(t, err)
	assert.Zero(t, secretReadsOrDeletes)
	assert.Equal(t, []string{"create:pods"}, actionSummaries(client.Actions()))
}

func TestDeployAgentSecretCreateFailureDeletesPod(t *testing.T) {
	p, client := newTestProvider("", testConfig())
	client.PrependReactor("create", "secrets", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("secret create rejected")
	})

	err := p.DeployAgent(t.Context(), &woodpecker.Agent{ID: 9, Name: "pool-7-agent-ABC", Token: "token"})
	require.Error(t, err)
	_, err = client.CoreV1().Pods(testNamespace).Get(t.Context(), "pool-7-agent-abc-9", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err))
}

func TestRemoveAgent(t *testing.T) {
	p, client := newTestProvider("", testConfig())
	agent := &woodpecker.Agent{ID: 12, Name: "pool-7-agent-AbC", Token: "token"}
	require.NoError(t, p.DeployAgent(t.Context(), agent))
	require.NoError(t, p.RemoveAgent(t.Context(), agent))
	require.NoError(t, p.RemoveAgent(t.Context(), agent))

	_, err := client.CoreV1().Pods(testNamespace).Get(t.Context(), "pool-7-agent-abc-12", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err))
	assert.Equal(t, []string{"create:pods", "create:secrets", "delete:pods", "delete:pods", "get:pods"}, actionSummaries(client.Actions()))
}

func TestRemoveAgentWithoutIDFindsPodByAnnotation(t *testing.T) {
	p, client := newTestProvider("", testConfig())
	podName := "pool-7-agent-abc-99"
	_, err := client.CoreV1().Pods(testNamespace).Create(t.Context(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        podName,
			Namespace:   testNamespace,
			Labels:      map[string]string{engine.LabelPool: testPoolID},
			Annotations: map[string]string{agentNameAnnotation: "pool-7-agent-AbC"},
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = client.CoreV1().Pods(testNamespace).Create(t.Context(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "different-agent",
			Namespace:   testNamespace,
			Labels:      map[string]string{engine.LabelPool: testPoolID},
			Annotations: map[string]string{agentNameAnnotation: "pool-7-agent-Other"},
		},
	}, metav1.CreateOptions{})
	require.NoError(t, err)

	require.NoError(t, p.RemoveAgent(t.Context(), &woodpecker.Agent{Name: "pool-7-agent-AbC"}))
	_, err = client.CoreV1().Pods(testNamespace).Get(t.Context(), podName, metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err))
	_, err = client.CoreV1().Pods(testNamespace).Get(t.Context(), "different-agent", metav1.GetOptions{})
	assert.NoError(t, err, "same-pool pod with a different agent name must remain")
}

func TestListDeployedAgentNames(t *testing.T) {
	p, client := newTestProvider("", testConfig())
	for _, pod := range []*corev1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "pod-one",
				Namespace:   testNamespace,
				Labels:      map[string]string{engine.LabelPool: testPoolID},
				Annotations: map[string]string{agentNameAnnotation: "pool-7-agent-AbC"},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "other-pool",
				Namespace: testNamespace,
				Labels:    map[string]string{engine.LabelPool: "pool-8"},
				Annotations: map[string]string{
					agentNameAnnotation: "pool-8-agent-abc",
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "no-agent-name",
				Namespace: testNamespace,
				Labels:    map[string]string{engine.LabelPool: testPoolID},
			},
		},
	} {
		_, err := client.CoreV1().Pods(testNamespace).Create(t.Context(), pod, metav1.CreateOptions{})
		require.NoError(t, err)
	}

	names, err := p.ListDeployedAgentNames(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"pool-7-agent-AbC"}, names)
}

func TestLoadPodTemplate(t *testing.T) {
	tests := []struct {
		name        string
		template    string
		wantErrText string
	}{
		{
			name:        "reserved label",
			template:    "apiVersion: v1\nkind: Pod\nmetadata:\n  labels:\n    wp.autoscaler/custom: forbidden\nspec:\n  containers:\n    - name: agent\n      image: agent:v1\n",
			wantErrText: `kubernetes pod template: reserved label prefix "wp.autoscaler/"`,
		},
		{
			name:        "reserved annotation",
			template:    "apiVersion: v1\nkind: Pod\nmetadata:\n  annotations:\n    wp.autoscaler/custom: forbidden\nspec:\n  containers:\n    - name: agent\n      image: agent:v1\n",
			wantErrText: `kubernetes pod template: reserved annotation prefix "wp.autoscaler/"`,
		},
		{
			name:        "missing containers",
			template:    "apiVersion: v1\nkind: Pod\nmetadata: {}\nspec: {}\n",
			wantErrText: "kubernetes pod template has no containers",
		},
		{
			name:        "unknown field",
			template:    "apiVersion: v1\nkind: Pod\nmetadata: {}\nspec:\n  unknownField: true\n  containers:\n    - name: agent\n      image: agent:v1\n",
			wantErrText: "decode kubernetes pod template",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "pod.yaml")
			require.NoError(t, os.WriteFile(path, []byte(tt.template), 0o600))
			_, err := loadPodTemplate(path)
			assert.ErrorContains(t, err, tt.wantErrText)
		})
	}
}

func newTestProvider(templateImage string, cfg *config.Config) (*provider, *fake.Clientset) {
	client := fake.NewSimpleClientset()
	podTemplate := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      map[string]string{"template-label": "kept"},
			Annotations: map[string]string{"template-annotation": "custom"},
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: "woodpecker",
			Containers: []corev1.Container{{
				Name:  "agent",
				Image: templateImage,
				Env: []corev1.EnvVar{
					{Name: "TEMPLATE_ENV", Value: "kept"},
					{Name: woodpeckerAgentSecret, Value: "template-token"},
					{Name: woodpeckerAgentSecretFile, Value: "template-file-token"},
				},
			}},
		},
	}
	return &provider{
		client:      client,
		namespace:   testNamespace,
		config:      cfg,
		podTemplate: podTemplate,
	}, client
}

func actionSummaries(actions []ktesting.Action) []string {
	summaries := make([]string, 0, len(actions))
	for _, action := range actions {
		summaries = append(summaries, action.GetVerb()+":"+action.GetResource().Resource)
	}
	return summaries
}

func testConfig() *config.Config {
	return &config.Config{
		PoolID:            testPoolID,
		Image:             "configured-agent:v2",
		WorkflowsPerAgent: 3,
		GRPCAddress:       "woodpecker-server:9000",
		Environment:       map[string]string{"CUSTOM_ENV": "value"},
		ExtraAgentLabels:  map[string]string{"team": "ci"},
	}
}

func envValue(env []corev1.EnvVar, name string) string {
	for _, variable := range env {
		if variable.Name == name {
			return variable.Value
		}
	}
	return ""
}
