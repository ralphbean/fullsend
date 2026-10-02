package cli

import (
	"context"
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
