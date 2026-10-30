package main

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// 构建 Cobra 命令树并绑定部署参数
func newCommand(out, logs io.Writer, deploy func(options) error) (*cobra.Command, *options) {
	o := &options{}

	// 读取产物路径、校验参数并调用部署入口
	execute := func(cmd *cobra.Command, args []string) error {
		o.Path = "."
		if len(args) > 0 {
			o.Path = args[0]
		}
		if err := o.validate(); err != nil {
			return err
		}
		return deploy(*o)
	}

	// 配置根命令及无参数时的帮助行为
	root := &cobra.Command{
		Use:     "edgeone-makers-deployer",
		Short:   "Deploy built websites to EdgeOne Makers",
		Long:    "Upload a built directory or ZIP to EdgeOne Makers. No Node.js runtime is required.",
		Example: "  edgeone-makers-deployer deploy dist -n my-site --site china\n  edgeone-makers-deployer deploy site.zip --project-id PROJECT_ID --json",
		Args:    cobra.MaximumNArgs(1), SilenceUsage: true, SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 && cmd.Flags().NFlag() == 0 {
				return cmd.Help()
			}
			return execute(cmd, args)
		},
	}
	root.SetOut(out)
	root.SetErr(logs)
	f := root.PersistentFlags()

	// 将官方 CLI 的双横线短别名映射为标准参数名
	root.SetGlobalNormalizationFunc(func(_ *pflag.FlagSet, name string) pflag.NormalizedName {
		switch name {
		case "n":
			name = "name"
		case "t":
			name = "token"
		case "e":
			name = "env"
		case "a":
			name = "area"
		}
		return pflag.NormalizedName(name)
	})

	// 项目选择与认证参数
	f.StringVarP(&o.Name, "name", "n", "", "Project name; create it if missing")
	f.StringVar(&o.ProjectID, "project-id", "", "Existing project ID (takes precedence over name)")
	f.StringVarP(&o.Token, "token", "t", "", "Makers API token (defaults to environment or local auth)")

	// 部署环境与 AI Gateway 参数
	f.StringVarP(&o.Env, "env", "e", "production", "Deployment environment: production or preview")
	f.StringVarP(&o.Area, "area", "a", "global", "New project area: global or overseas")
	f.StringVar(&o.Site, "site", "", "Token site: china or global (auto-detect if omitted)")
	f.BoolVar(&o.Anonymous, "anonymous", false, "Temporary anonymous deployment when no token is available")
	f.BoolVar(&o.SkipAI, "skip-ai-gateway-sync", false, "Skip binding AI Gateway cloud credentials")

	// 输出格式与等待方式
	f.BoolVar(&o.JSON, "json", false, "Write one JSON result to stdout; logs go to stderr")
	f.BoolVar(&o.NoWait, "no-wait", false, "Return after creation, before publication completes")

	// 压缩、分片和传输加速参数
	f.IntVar(&o.Compression, "compression", 1, "ZIP compression level 0–9 (0 stores files)")
	f.IntVar(&o.Concurrency, "concurrency", 8, "Parallel COS upload parts (1–64)")
	f.Int64Var(&o.PartSize, "part-size", 8, "COS part size in MiB (1–5120)")
	f.BoolVar(&o.Accelerate, "accelerate", true, "Use COS transfer acceleration")

	// 部署期限与状态查询间隔
	f.DurationVar(&o.Timeout, "timeout", 15*time.Minute, "Total deadline including packaging and upload")
	f.DurationVar(&o.RequestTimeout, "request-timeout", 30*time.Second, "API request and COS response-header timeout")
	f.DurationVar(&o.Poll, "poll-interval", 5*time.Second, "Deployment status polling interval")

	// 在命令执行前从环境变量补充 Token 和站点
	root.PersistentPreRun = func(cmd *cobra.Command, _ []string) {
		if !f.Changed("token") {
			o.Token = os.Getenv("EDGEONE_PAGES_API_TOKEN")
			if o.Token == "" {
				o.Token = os.Getenv("EDGEONE_MAKER_API_TOKEN")
			}
		}
		if !f.Changed("site") {
			o.Site = os.Getenv("EDGEONE_PAGES_API_REGION")
		}
	}

	// 注册 deploy 及 makers/pages 兼容入口
	makeDeploy := func() *cobra.Command {
		return &cobra.Command{Use: "deploy [directoryOrZip]", Short: "Upload a directory or ZIP and deploy it", Long: "Deploy an existing build output. The default path is the current directory; no build is run.", Example: root.Example, Args: cobra.MaximumNArgs(1), RunE: execute}
	}
	root.AddCommand(makeDeploy())
	legacy := &cobra.Command{Use: "makers", Aliases: []string{"pages"}, Short: "Official CLI command compatibility", Hidden: true}
	legacy.AddCommand(makeDeploy())
	root.AddCommand(legacy)
	root.SetUsageFunc(groupedUsage)
	return root, o
}

// 按功能分组输出命令帮助
func groupedUsage(cmd *cobra.Command) error {
	w := cmd.OutOrStdout()
	fmt.Fprintln(w, "Usage:")
	if cmd.HasAvailableSubCommands() {
		fmt.Fprintf(w, "  %s <command> [flags]\n", cmd.CommandPath())
	} else {
		fmt.Fprintf(w, "  %s\n", cmd.UseLine())
	}
	if cmd.HasAvailableSubCommands() {
		fmt.Fprintln(w, "\nCommands:")
		for _, c := range cmd.Commands() {
			if c.IsAvailableCommand() {
				fmt.Fprintf(w, "  %-14s %s\n", c.Name(), c.Short)
			}
		}
	}

	// 合并当前命令的本地参数与继承参数
	all := pflag.NewFlagSet("help", pflag.ContinueOnError)
	all.AddFlagSet(cmd.InheritedFlags())
	all.AddFlagSet(cmd.LocalFlags())

	// 定义帮助分组及参数展示顺序
	groups := []struct {
		title string
		names []string
	}{
		{"Project & authentication", []string{"name", "project-id", "token", "site", "anonymous"}},
		{"Deployment", []string{"env", "area", "skip-ai-gateway-sync"}},
		{"Upload tuning", []string{"compression", "concurrency", "part-size", "accelerate"}},
		{"Waiting & output", []string{"timeout", "request-timeout", "poll-interval", "no-wait", "json"}},
	}

	// 逐组输出参数，并记录已展示的名称
	used := map[string]bool{}
	for _, g := range groups {
		set := pflag.NewFlagSet(g.title, pflag.ContinueOnError)
		set.SortFlags = false
		for _, name := range g.names {
			if f := all.Lookup(name); f != nil && !f.Hidden {
				set.AddFlag(f)
				used[name] = true
			}
		}
		if set.HasFlags() {
			fmt.Fprintf(w, "\n%s:\n%s", g.title, set.FlagUsagesWrapped(100))
		}
	}

	// 输出帮助等其余参数和使用示例
	other := pflag.NewFlagSet("other", pflag.ContinueOnError)
	all.VisitAll(func(f *pflag.Flag) {
		if !used[f.Name] && !f.Hidden {
			other.AddFlag(f)
		}
	})
	if other.HasFlags() {
		fmt.Fprintf(w, "\nOther flags:\n%s", other.FlagUsagesWrapped(100))
	}
	if cmd.HasExample() {
		fmt.Fprintf(w, "\nExamples:\n%s\n", cmd.Example)
	}
	return nil
}
