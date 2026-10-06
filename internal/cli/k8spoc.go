package cli

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/fullsend-ai/fullsend/internal/k8spoc"
	"github.com/spf13/cobra"
)

func newK8sPoCCmd() *cobra.Command {
	root := &cobra.Command{Use: "k8s-poc", Short: "Run the Kubernetes dispatcher proof of concept"}
	root.AddCommand(&cobra.Command{Use: "migrate", Short: "Apply versioned PoC database schema", RunE: func(cmd *cobra.Command, _ []string) error { return k8spoc.Migrate(cmd.Context()) }})
	var fixture, selection string
	poll := &cobra.Command{Use: "poll-fixture", Short: "Commit fixture observations and checkpoint to PostgreSQL", RunE: func(cmd *cobra.Command, _ []string) error { return k8spoc.Poll(cmd.Context(), fixture, selection) }}
	poll.Flags().StringVar(&fixture, "fixture", "/etc/fullsend/fixture.json", "Fixture source JSON file")
	poll.Flags().StringVar(&selection, "selection", "/etc/fullsend/selection.json", "Fixture selection JSON file")
	root.AddCommand(poll)
	var liveConfig, jiraTokenFile, jiraEmailFile string
	livePoll := &cobra.Command{Use: "poll-jira", Short: "Commit bounded Jira observations and checkpoint to PostgreSQL", RunE: func(cmd *cobra.Command, _ []string) error {
		return k8spoc.PollJira(cmd.Context(), liveConfig, jiraTokenFile, jiraEmailFile)
	}}
	livePoll.Flags().StringVar(&liveConfig, "config", "/etc/fullsend/live.json", "Validated live selection snapshot")
	livePoll.Flags().StringVar(&jiraTokenFile, "jira-token-file", "/var/run/secrets/jira/token", "Jira API token file")
	livePoll.Flags().StringVar(&jiraEmailFile, "jira-email-file", "/var/run/secrets/jira/email", "Jira account email file")
	root.AddCommand(livePoll)
	var manifestPath, repoDir, repoSlug, jiraURL, project, label, outputPath string
	prepare := &cobra.Command{Use: "prepare-live", Short: "Validate and snapshot a Jira source and repository harnesses", RunE: func(cmd *cobra.Command, _ []string) error {
		return k8spoc.PrepareLiveConfig(cmd.Context(), manifestPath, repoDir, repoSlug, jiraURL, project, label, outputPath)
	}}
	prepare.Flags().StringVar(&manifestPath, "repos-manifest", "", "Path to a repos.yaml manifest")
	prepare.Flags().StringVar(&repoDir, "repo-dir", "", "Path to the trusted repository checkout")
	prepare.Flags().StringVar(&repoSlug, "repo", "", "Registered owner/repo slug")
	prepare.Flags().StringVar(&jiraURL, "jira-url", "", "Jira Cloud base URL")
	prepare.Flags().StringVar(&project, "jira-project", "", "Jira project key")
	prepare.Flags().StringVar(&label, "jira-label", "", "Exact Jira scope label")
	prepare.Flags().StringVar(&outputPath, "output", "", "Output path for validated immutable snapshot")
	root.AddCommand(prepare)
	root.AddCommand(&cobra.Command{Use: "dispatch", Short: "Select pending work, reconcile Jobs, and launch bounded runs", RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, cancel := context.WithTimeout(cmd.Context(), 45*time.Second)
		defer cancel()
		return k8spoc.Dispatch(ctx)
	}})
	root.AddCommand(&cobra.Command{Use: "status", Short: "Print durable PoC work counts", RunE: func(cmd *cobra.Command, _ []string) error { return k8spoc.Status(cmd.Context()) }})
	var probeName, probeImage, probeOutput string
	probe := &cobra.Command{Use: "gateway-probe", Short: "Exercise two remote sandboxes through the provisioned gateway", RunE: func(cmd *cobra.Command, _ []string) error {
		return k8spoc.GatewayProbe(cmd.Context(), probeName, probeImage, probeOutput)
	}}
	probe.Flags().StringVar(&probeName, "name", "", "Unique fs- prefix for two probe sandboxes")
	probe.Flags().StringVar(&probeImage, "sandbox-image", "", "Sandbox image pinned by sha256 digest")
	probe.Flags().StringVar(&probeOutput, "output-dir", "", "Persistent directory for intent and transfer evidence")
	_ = probe.MarkFlagRequired("name")
	_ = probe.MarkFlagRequired("sandbox-image")
	_ = probe.MarkFlagRequired("output-dir")
	root.AddCommand(probe)
	var jobName, jobProbeName, runnerImage, sandboxImage string
	renderProbe := &cobra.Command{Use: "render-gateway-probe-job", Short: "Render a runner Job using the dispatcher template", RunE: func(cmd *cobra.Command, _ []string) error {
		body, err := k8spoc.GatewayProbeJob(jobName, jobProbeName, runnerImage, sandboxImage)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(os.Stdout, string(body))
		return err
	}}
	renderProbe.Flags().StringVar(&jobName, "job-name", "", "Unique Kubernetes Job name")
	renderProbe.Flags().StringVar(&jobProbeName, "probe-name", "", "Unique fs- prefix for remote sandbox names")
	renderProbe.Flags().StringVar(&runnerImage, "runner-image", "", "Runner image pinned by sha256 digest")
	renderProbe.Flags().StringVar(&sandboxImage, "sandbox-image", "", "Sandbox image pinned by sha256 digest")
	for _, flag := range []string{"job-name", "probe-name", "runner-image", "sandbox-image"} {
		_ = renderProbe.MarkFlagRequired(flag)
	}
	root.AddCommand(renderProbe)
	var demoJobName, demoSandboxName, demoDir, demoEvidence, demoRunnerImage string
	demo := &cobra.Command{Use: "gateway-demo", Short: "Run the pinned local harness through the gateway", RunE: func(cmd *cobra.Command, _ []string) error {
		return k8spoc.GatewayDemo(cmd.Context(), demoJobName, demoSandboxName, demoDir, demoEvidence)
	}}
	demo.Flags().StringVar(&demoJobName, "job-name", "", "Owned fs-demo-<hex> Kubernetes Job name")
	demo.Flags().StringVar(&demoSandboxName, "sandbox-name", "", "Owned fs-<hex> remote sandbox name")
	demo.Flags().StringVar(&demoDir, "demo-dir", "", "Directory containing the pinned demo harness and target repository")
	demo.Flags().StringVar(&demoEvidence, "evidence-dir", "", "Persistent directory for demo intent and results")
	for _, flag := range []string{"job-name", "sandbox-name", "demo-dir", "evidence-dir"} {
		_ = demo.MarkFlagRequired(flag)
	}
	root.AddCommand(demo)
	renderDemo := &cobra.Command{Use: "render-gateway-demo-job", Short: "Render a fullsend run Job using the dispatcher template", RunE: func(cmd *cobra.Command, _ []string) error {
		body, err := k8spoc.GatewayDemoJob(demoJobName, demoRunnerImage)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(os.Stdout, string(body))
		return err
	}}
	renderDemo.Flags().StringVar(&demoJobName, "job-name", "", "Unique fs-demo-<hex> Kubernetes Job name")
	renderDemo.Flags().StringVar(&demoRunnerImage, "runner-image", "", "Runner image pinned by digest or revision-and-content tag")
	_ = renderDemo.MarkFlagRequired("job-name")
	_ = renderDemo.MarkFlagRequired("runner-image")
	root.AddCommand(renderDemo)
	var cleanupEvidence, cleanupJobName, cleanupRunnerImage string
	cleanup := &cobra.Command{Use: "cleanup-gateway-probes", Short: "Delete remote probe sandboxes left by interrupted runner Jobs", RunE: func(cmd *cobra.Command, _ []string) error {
		return k8spoc.GatewayCleanup(cmd.Context(), cleanupEvidence)
	}}
	cleanup.Flags().StringVar(&cleanupEvidence, "evidence-dir", "", "Persistent probe evidence directory")
	_ = cleanup.MarkFlagRequired("evidence-dir")
	root.AddCommand(cleanup)
	renderCleanup := &cobra.Command{Use: "render-gateway-cleanup-job", Short: "Render a cleanup Job using the runner template", RunE: func(cmd *cobra.Command, _ []string) error {
		body, err := k8spoc.GatewayCleanupJob(cleanupJobName, cleanupRunnerImage)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(os.Stdout, string(body))
		return err
	}}
	renderCleanup.Flags().StringVar(&cleanupJobName, "job-name", "", "Unique cleanup Job name")
	renderCleanup.Flags().StringVar(&cleanupRunnerImage, "runner-image", "", "Runner image pinned by digest or revision-and-content tag")
	_ = renderCleanup.MarkFlagRequired("job-name")
	_ = renderCleanup.MarkFlagRequired("runner-image")
	root.AddCommand(renderCleanup)
	for _, action := range []struct {
		name, help string
		run        func(context.Context, int64) error
	}{
		{"retry", "Requeue one cleaned-up failed run", k8spoc.Retry},
		{"dismiss", "Acknowledge one cleaned-up failed run", k8spoc.Dismiss},
	} {
		action := action
		root.AddCommand(&cobra.Command{Use: action.name + " RUN_ID", Short: action.help, Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return err
			}
			return action.run(cmd.Context(), id)
		}})
	}
	return root
}
