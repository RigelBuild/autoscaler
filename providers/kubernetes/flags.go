package kubernetes

import "github.com/urfave/cli/v3"

const category = "Kubernetes"

var ProviderFlags = []cli.Flag{
	&cli.StringFlag{
		Name:     "kubernetes-namespace",
		Usage:    "namespace for Woodpecker agent pods (required for provider kubernetes)",
		Sources:  cli.EnvVars("WOODPECKER_KUBERNETES_NAMESPACE"),
		Category: category,
	},
	&cli.StringFlag{
		Name:     "kubernetes-pod-template-file",
		Usage:    "path to the base Kubernetes Pod YAML for Woodpecker agents (required for provider kubernetes)",
		Sources:  cli.EnvVars("WOODPECKER_KUBERNETES_POD_TEMPLATE_FILE"),
		Category: category,
	},
	&cli.StringFlag{
		Name:     "kubernetes-kubeconfig",
		Usage:    "path to a Kubernetes kubeconfig file; defaults to in-cluster configuration",
		Sources:  cli.EnvVars("WOODPECKER_KUBERNETES_KUBECONFIG"),
		Category: category,
	},
}
