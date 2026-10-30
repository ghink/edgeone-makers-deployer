package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// 部署参数
type options struct {
	Path, Name, Token, Env, Area, Site, ProjectID string
	JSON, Anonymous, SkipAI, NoWait, Accelerate   bool
	Timeout, RequestTimeout, Poll                 time.Duration
	Compression, Concurrency                      int
	PartSize                                      int64
}

// 校验参数并补充本地认证信息
func (o *options) validate() error {
	// 校验环境、区域和上传参数
	if o.Env != "production" && o.Env != "preview" {
		return errors.New("--env must be production or preview")
	}
	if o.Area != "global" && o.Area != "overseas" {
		return errors.New("--area must be global or overseas")
	}
	if o.Site != "" && o.Site != "china" && o.Site != "global" {
		return errors.New("--site must be china or global")
	}
	if o.Compression < 0 || o.Compression > 9 || o.Concurrency < 1 || o.Concurrency > 64 || o.PartSize < 1 || o.PartSize > 5120 {
		return errors.New("invalid compression, concurrency, or part-size")
	}
	if o.Timeout <= 0 || o.RequestTimeout <= 0 || o.Poll <= 0 {
		return errors.New("timeouts and poll interval must be positive")
	}

	// 从本地认证文件补充 Token 和站点
	if o.Token == "" {
		var auth struct {
			Token  string `json:"EDGEONE_PAGES_API_TOKEN"`
			Region string `json:"EDGEONE_PAGES_API_REGION"`
		}
		if b, e := os.ReadFile(".edgeone/auth.json"); e == nil && json.Unmarshal(b, &auth) == nil {
			o.Token = auth.Token
			if o.Site == "" && (auth.Region == "china" || auth.Region == "global") {
				o.Site = auth.Region
			}
		}
	}

	// 校验认证要求和匿名部署限制
	if o.Token == "" && !o.Anonymous {
		return errors.New("provide --token or EDGEONE_PAGES_API_TOKEN (or --anonymous)")
	}
	if o.Anonymous && o.Token == "" && (o.Env != "production" || o.Name != "" || o.ProjectID != "" || o.Area != "global") {
		return errors.New("anonymous API does not support name, project-id, preview, or area selection")
	}
	return nil
}

// 程序入口
func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

// 执行命令并返回对应退出码
func run(args []string, out, logs io.Writer) int {
	deployed := false
	cmd, current := newCommand(out, logs, func(o options) error { deployed = true; return executeDeployment(o, out, logs) })
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		j := current != nil && current.JSON
		emitError(out, logs, j, err)
		if deployed {
			return 1
		}
		return 2
	}
	return 0
}

// 执行部署并输出结果与耗时
func executeDeployment(o options, out, logs io.Writer) error {
	// 设置中断处理和部署总期限
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	start := time.Now()

	// 执行部署流程并对错误脱敏
	d := newDeployer(o, logs)
	result, err := d.deploy(ctx)
	if err != nil {
		return errors.New(d.redact(err.Error()))
	}

	// 附加计时信息并输出部署结果
	result["durationSeconds"] = time.Since(start).Seconds()
	result["timings"] = d.timings
	if o.JSON {
		json.NewEncoder(out).Encode(result)
	} else {
		fmt.Fprintln(out, "Deployment:", result["status"])
		for _, k := range []string{"projectId", "deploymentId", "url", "consoleUrl", "claimUrl"} {
			if v, ok := result[k]; ok && v != "" {
				fmt.Fprintf(out, "%s: %v\n", k, v)
			}
		}
	}
	return nil
}

// 按文本或 JSON 格式输出错误
func emitError(out, logs io.Writer, j bool, err error) {
	if j {
		json.NewEncoder(out).Encode(map[string]any{"status": "error", "error": err.Error()})
	} else {
		fmt.Fprintln(logs, "Error:", err)
	}
}
