package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"
	"time"
)

// API 响应对象
type object map[string]any

// 读取响应中的字符串、对象和记录列表
func str(m object, k string) string { s, _ := m[k].(string); return s }

func obj(m object, k string) object {
	switch v := m[k].(type) {
	case object:
		return v
	case map[string]any:
		return object(v)
	}
	return nil
}

func records(m object, k string) []object {
	var out []object
	a, _ := m[k].([]any)
	for _, v := range a {
		if x, ok := v.(map[string]any); ok {
			out = append(out, object(x))
		}
	}
	return out
}

// 部署客户端、传输配置和计时状态
type deployer struct {
	cosTransport   http.RoundTripper
	uploadArtifact func(context.Context, string, object, string) error
	o              options
	http           *http.Client
	logs           io.Writer
	site, base     string
	secrets        []string
	timings        map[string]float64
}

// 初始化 HTTP 客户端与部署状态
func newDeployer(o options, w io.Writer) *deployer {
	return &deployer{o: o, http: &http.Client{Timeout: o.RequestTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, logs: w, secrets: []string{o.Token}, timings: map[string]float64{}}
}

// 替换日志和错误中的敏感凭证
func (d *deployer) redact(s string) string {
	for _, v := range d.secrets {
		if v != "" {
			s = strings.ReplaceAll(s, v, "[REDACTED]")
		}
	}
	return s
}

// 输出脱敏后的日志
func (d *deployer) log(f string, a ...any) { fmt.Fprintln(d.logs, d.redact(fmt.Sprintf(f, a...))) }

// 记录阶段开始时间并返回结束计时函数
func (d *deployer) stage(name string) func() {
	t := time.Now()
	d.log("%s...", name)
	return func() { v := time.Since(t).Seconds(); d.timings[name] += v; d.log("%s: %.2fs", name, v) }
}

// 返回指定站点的 API 地址
func endpoint(site string) string {
	if site == "china" {
		return "https://pages-api.cloud.tencent.com"
	}
	return "https://pages-api.edgeone.ai"
}

// 发送 JSON 请求并读取响应
func (d *deployer) post(ctx context.Context, url string, body object, auth bool) (object, error) {
	// 构造请求体并按需添加认证头
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if auth {
		req.Header.Set("Authorization", "Bearer "+d.o.Token)
	}

	// 发送请求并检查 HTTP 状态
	res, err := d.http.Do(req)
	if err != nil {
		return nil, errors.New("API transport failure: " + d.redact(err.Error()))
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
		return nil, fmt.Errorf("API HTTP %d", res.StatusCode)
	}

	// 限制响应大小并解析 JSON
	var m object
	if err = json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(&m); err != nil {
		return nil, fmt.Errorf("invalid API JSON: %w", err)
	}
	return m, nil
}

// 提取认证或匿名接口的数据并检查业务错误
func unwrap(m object, anonymous bool) (object, error) {
	key := "Code"
	data := "Data"
	if anonymous {
		key = "code"
		data = "data"
	}
	if code, ok := m[key]; ok && fmt.Sprint(code) != "0" {
		return nil, fmt.Errorf("API error %v: %s", code, str(m, map[bool]string{true: "message", false: "Message"}[anonymous]))
	}
	r := m
	if v := obj(m, data); v != nil {
		r = v
	}
	if v := obj(r, "Response"); v != nil {
		r = v
	}
	if e := obj(r, "Error"); e != nil {
		return nil, fmt.Errorf("API %s: %s", str(e, "Code"), str(e, "Message"))
	}
	return r, nil
}

// 调用需要 Token 的 Makers Action 接口
func (d *deployer) api(ctx context.Context, action string, body object) (object, error) {
	b := object{"Action": action}
	maps.Copy(b, body)
	m, err := d.post(ctx, d.base+"/v1", b, true)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	r, err := unwrap(m, false)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	return r, nil
}

// 调用匿名部署接口并返回原始数据与业务响应
func (d *deployer) public(ctx context.Context, action string, body object) (object, object, error) {
	m, err := d.post(ctx, d.base+"/pages-public/"+action, body, false)
	if err != nil {
		return nil, nil, err
	}
	r, err := unwrap(m, true)
	return obj(m, "data"), r, err
}

// 等待指定间隔并响应取消信号
func pause(ctx context.Context, t time.Duration) error {
	timer := time.NewTimer(t)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// 选择部署站点并设置 API 地址
func (d *deployer) resolveSite(ctx context.Context) error {
	// 优先使用显式指定的站点
	if d.o.Site != "" {
		d.site = d.o.Site
		d.base = endpoint(d.site)
		return nil
	}

	// 匿名部署通过 IP 探测站点，探测失败时使用国际站
	if d.o.Token == "" {
		d.site = "global"
		probe, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(probe, "GET", "https://api.edgeone.ai/e-func/ip/isCN", nil)
		res, err := d.http.Do(req)
		if err == nil {
			defer res.Body.Close()
			var m object
			if json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&m) == nil && m["isCN"] == true {
				d.site = "china"
			}
		}
		d.base = endpoint(d.site)
		return nil
	}

	// 认证部署依次探测中国站和国际站
	var failures []string
	for _, site := range []string{"china", "global"} {
		d.base = endpoint(site)
		if _, err := d.api(ctx, "DescribePagesProjects", object{"Offset": 0, "Limit": 1}); err == nil {
			d.site = site
			return nil
		} else {
			failures = append(failures, site+": "+err.Error())
		}
	}
	return fmt.Errorf("cannot resolve token site (%s); use --site to avoid probing", strings.Join(failures, "; "))
}
