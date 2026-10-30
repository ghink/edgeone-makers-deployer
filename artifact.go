package main

import (
	"archive/zip"
	"compress/flate"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// 检查归档路径是否为安全的相对路径
func safeEntry(name string) bool {
	if name == "" || strings.ContainsAny(name, "\\\x00") || strings.HasPrefix(name, "/") {
		return false
	}
	for s := range strings.SplitSeq(name, "/") {
		if s == ".." || strings.Contains(s, ":") {
			return false
		}
	}
	return true
}

// 检查 ZIP 中的路径、符号链接和重复条目
func validateZip(file string) error {
	r, e := zip.OpenReader(file)
	if e != nil {
		return e
	}
	defer r.Close()
	seen := map[string]bool{}
	for _, f := range r.File {
		if !safeEntry(f.Name) || f.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("unsafe ZIP entry %q", f.Name)
		}
		n := path.Clean(f.Name)
		if seen[n] {
			return fmt.Errorf("duplicate ZIP entry %q", n)
		}
		seen[n] = true
	}
	if len(r.File) == 0 {
		return errors.New("empty ZIP")
	}
	return nil
}

// 校验现有 ZIP，或将目录打包为临时 ZIP
func (d *deployer) prepare(ctx context.Context) (string, func(), error) {
	// 检查产物路径和文件类型
	abs, err := filepath.Abs(d.o.Path)
	if err != nil {
		return "", nil, err
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return "", nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", nil, errors.New("artifact must not be a symlink")
	}

	// 现有 ZIP 校验后直接复用
	if !info.IsDir() {
		if !info.Mode().IsRegular() || !strings.EqualFold(filepath.Ext(abs), ".zip") {
			return "", nil, errors.New("artifact must be directory or ZIP")
		}
		err = validateZip(abs)
		return abs, func() {}, err
	}

	// 创建临时归档并配置压缩等级
	f, err := os.CreateTemp("", "edgeone-deploy-*.zip")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { os.Remove(f.Name()) }
	z := zip.NewWriter(f)
	z.RegisterCompressor(zip.Deflate, func(w io.Writer) (io.WriteCloser, error) { return flate.NewWriter(w, d.o.Compression) })

	// 设置普通目录或 .edgeone 产物的归档前缀
	edge := filepath.Base(abs) == ".edgeone"
	prefix := filepath.Base(abs)
	if edge {
		prefix = filepath.Base(filepath.Dir(abs)) + "/.edgeone"
	}
	count := 0

	// 将单个文件写入归档并累计文件数
	add := func(source, name string, info fs.FileInfo) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !safeEntry(name) {
			return fmt.Errorf("invalid file path %q", name)
		}
		h, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		h.Name = name
		h.Method = zip.Deflate
		if d.o.Compression == 0 {
			h.Method = zip.Store
		}
		w, err := z.CreateHeader(h)
		if err != nil {
			return err
		}
		r, err := os.Open(source)
		if err != nil {
			return err
		}
		defer r.Close()
		_, err = io.Copy(w, &contextReader{ctx, r})
		if err == nil {
			count++
		}
		return err
	}

	// 遍历目录，过滤排除项并写入常规文件
	err = filepath.WalkDir(abs, func(p string, e fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if p == abs {
			return nil
		}
		rel, _ := filepath.Rel(abs, p)
		rel = filepath.ToSlash(rel)
		skip := (edge && p == filepath.Join(abs, "auth.json")) || e.Name() == ".git" || e.Name() == ".cache" || e.Name() == ".DS_Store" || strings.HasSuffix(e.Name(), ".log") || strings.HasSuffix(e.Name(), ".tmp") || (!edge && (e.Name() == "node_modules" || e.Name() == ".edgeone" || e.Name() == ".env" || strings.HasPrefix(e.Name(), ".env.")))
		if skip {
			if e.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if e.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink rejected: %s", rel)
		}
		if e.IsDir() {
			return nil
		}
		i, err := e.Info()
		if err != nil {
			return err
		}
		if !i.Mode().IsRegular() {
			return fmt.Errorf("nonregular file rejected: %s", rel)
		}
		return add(p, prefix+"/"+rel, i)
	})

	// 为 .edgeone 产物加入同级配置文件
	if err == nil && edge {
		config := filepath.Join(filepath.Dir(abs), "edgeone.json")
		if i, e := os.Stat(config); e == nil {
			err = add(config, filepath.Base(filepath.Dir(abs))+"/edgeone.json", i)
		} else if !os.IsNotExist(e) {
			err = e
		}
	}

	// 关闭归档，检查空目录并清理失败产物
	if e := z.Close(); err == nil {
		err = e
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err == nil && count == 0 {
		err = errors.New("artifact directory has no files")
	}
	if err != nil {
		cleanup()
		return "", nil, err
	}
	d.log("Packaged %d files", count)
	return f.Name(), cleanup, nil
}

// 支持取消操作的文件读取器
type contextReader struct {
	ctx context.Context
	r   io.Reader
}

// 读取前检查上下文是否已取消
func (r *contextReader) Read(b []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	return r.r.Read(b)
}

// 从归档的 edgeone.json 读取并校验 Agent 框架
func agentFramework(file string) (string, error) {
	r, err := zip.OpenReader(file)
	if err != nil {
		return "", err
	}
	defer r.Close()

	// 查找归档根目录或一层目录下的配置
	var configs []*zip.File
	for _, f := range r.File {
		parts := strings.Split(strings.TrimSuffix(f.Name, "/"), "/")
		if len(parts) <= 2 && strings.EqualFold(parts[len(parts)-1], "edgeone.json") {
			configs = append(configs, f)
		}
	}
	if len(configs) == 0 {
		return "", nil
	}
	if len(configs) > 1 {
		return "", errors.New("ambiguous edgeone.json files; use --skip-ai-gateway-sync to bypass binding")
	}

	// 检查配置大小并读取 Agent 框架
	f := configs[0]
	if f.UncompressedSize64 > 65536 {
		return "", errors.New("edgeone.json exceeds 64 KiB")
	}
	rd, err := f.Open()
	if err != nil {
		return "", err
	}
	defer rd.Close()
	var cfg struct {
		Agents struct {
			Framework string `json:"framework"`
		} `json:"agents"`
	}
	if err = json.NewDecoder(io.LimitReader(rd, 65537)).Decode(&cfg); err != nil {
		return "", err
	}
	framework := strings.TrimSpace(cfg.Agents.Framework)
	if framework != "" {
		switch framework {
		case "claude-agent-sdk", "openai-agents-sdk", "langgraph", "crewai", "deepagents":
		default:
			return "", fmt.Errorf("unsupported agent framework %q", framework)
		}
	}
	return framework, nil
}
