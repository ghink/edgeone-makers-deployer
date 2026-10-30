package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"hash/crc64"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// 生成通用测试参数
func testOptions(t *testing.T) options {
	t.Helper()
	o, e := parse([]string{"deploy", "dist", "--name", "example", "--token", "test-secret", "--site", "china"})
	if e != nil {
		t.Fatal(e)
	}
	o.Poll = time.Millisecond
	return o
}

// 验证官方 CLI 参数别名与参数校验
func TestParseCompatibility(t *testing.T) {
	t.Setenv("EDGEONE_PAGES_API_TOKEN", "")
	t.Setenv("EDGEONE_MAKER_API_TOKEN", "")
	for _, args := range [][]string{{"makers", "deploy", "dist", "--n", "example", "--t", "token", "--e", "preview", "--a", "overseas", "--json", "--skip-ai-gateway-sync", "--site", "china"}, {"deploy", "-n=example", "-t=token", "dist", "-e=preview", "-a=overseas", "--json=true", "--skip-ai-gateway-sync=true", "--site=china"}} {
		o, e := parse(args)
		if e != nil {
			t.Fatal(e)
		}
		if o.Name != "example" || o.Path != "dist" || o.Env != "preview" || o.Area != "overseas" || !o.JSON || !o.SkipAI {
			t.Fatalf("bad options %+v", o)
		}
	}
	for _, args := range [][]string{{"--token", "x", "--env", "bad"}, {"--token", "x", "--concurrency", "0"}, {"--token", "x", "--timeout", "0s"}, {"--anonymous", "--env", "preview"}, {"--token", "x", "--wat"}, {"--token"}} {
		if _, e := parse(args); e == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

// 创建最小静态网站产物
func writeFixture(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "dist")
	if e := os.Mkdir(p, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(p, "index.html"), []byte("hello"), 0600); e != nil {
		t.Fatal(e)
	}
	return p
}

// 验证目录打包、排除规则和取消操作
func TestPackagingAndCancellation(t *testing.T) {
	o := testOptions(t)
	o.Path = writeFixture(t)
	os.Mkdir(filepath.Join(o.Path, "node_modules"), 0700)
	os.WriteFile(filepath.Join(o.Path, "node_modules", "secret"), []byte("secret"), 0600)
	os.WriteFile(filepath.Join(o.Path, ".well-known"), []byte("ok"), 0600)
	d := newDeployer(o, io.Discard)
	p, clean, e := d.prepare(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	defer clean()
	r, e := zip.OpenReader(p)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	if len(r.File) != 2 {
		t.Fatalf("entries %+v", r.File)
	}
	for _, f := range r.File {
		if strings.Contains(f.Name, "node_modules") {
			t.Fatal("excluded directory leaked")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, e = d.prepare(ctx); !errors.Is(e, context.Canceled) {
		t.Fatalf("cancellation: %v", e)
	}
	os.Symlink("index.html", filepath.Join(o.Path, "link"))
	if _, _, e = d.prepare(context.Background()); e == nil {
		t.Fatal("accepted symlink")
	}
}

// 验证不安全归档路径与重复条目的拒绝行为
func TestRejectUnsafeZIP(t *testing.T) {
	for _, names := range [][]string{{"../x"}, {"/x"}, {"a\\b"}, {"a", "./a"}} {
		p := filepath.Join(t.TempDir(), "bad.zip")
		f, _ := os.Create(p)
		w := zip.NewWriter(f)
		for _, n := range names {
			x, _ := w.Create(n)
			x.Write([]byte("x"))
		}
		w.Close()
		f.Close()
		if validateZip(p) == nil {
			t.Fatalf("accepted %v", names)
		}
	}
}

// 将测试函数适配为 HTTP 传输器
type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// 生成模拟 API 的 JSON 响应
func respond(v any) *http.Response {
	b, _ := json.Marshal(v)
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(b))}
}

// 模拟认证部署并检查请求顺序和预览结果
func TestAuthenticatedDeployment(t *testing.T) {
	o := testOptions(t)
	o.Path = writeFixture(t)
	o.Env = "preview"
	var logs bytes.Buffer
	d := newDeployer(o, &logs)
	var calls []string
	polls := 0
	uploaded := false
	d.uploadArtifact = func(ctx context.Context, file string, c object, key string) error {
		uploaded = true
		if !strings.HasPrefix(key, "temp/") || validateZip(file) != nil {
			t.Fatal("invalid artifact")
		}
		return nil
	}
	d.http.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer test-secret" {
			t.Fatal("missing auth")
		}
		var b object
		json.NewDecoder(r.Body).Decode(&b)
		a := str(b, "Action")
		calls = append(calls, a)
		result := object{}
		switch a {
		case "DescribePagesProjects":
			result = object{"Projects": []object{{"ProjectId": "p1", "Provider": "Upload", "Area": "overseas", "PresetDomain": "site.example"}}}
		case "DescribePagesCosTempToken":
			result = object{"TargetPath": "temp"}
		case "CreatePagesDeployment":
			if !uploaded || str(b, "Env") != "Preview" || str(b, "DistType") != "Zip" {
				t.Fatalf("bad create: %v", b)
			}
			result = object{"DeploymentId": "d1"}
		case "DescribePagesDeployments":
			if !uploaded {
				result = object{"Deployments": []object{{"UsedInProd": true}}}
			} else {
				polls++
				status := "Pending"
				if polls > 1 {
					status = "Success"
				}
				result = object{"Deployments": []object{{"DeploymentId": "d1", "Status": status, "PreviewUrl": "preview.example"}}}
			}
		default:
			t.Fatalf("unexpected action %s", a)
		}
		return respond(object{"Code": 0, "Data": object{"Response": result}}), nil
	})
	result, e := d.deploy(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	if result["url"] != "https://preview.example" || result["status"] != "success" {
		t.Fatal(result)
	}
	if polls != 2 || strings.Contains(logs.String(), "test-secret") {
		t.Fatal("polling/redaction failed")
	}
	if len(calls) != 7 {
		t.Fatalf("unexpected calls: %v", calls)
	}
}

// 验证轮询失败上限、脱敏和部署期限
func TestPollingFailuresAndDeadline(t *testing.T) {
	d := newDeployer(testOptions(t), io.Discard)
	d.base = "https://example.invalid"
	calls := 0
	d.http.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("test-secret network failure")
	})
	if _, e := d.wait(context.Background(), "p", "d", ""); e == nil || calls != 3 || strings.Contains(e.Error(), "test-secret") {
		t.Fatalf("calls=%d error=%v", calls, e)
	}
	d.http.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		return respond(object{"Code": 0, "Data": object{"Response": object{"Deployments": []object{{"DeploymentId": "d", "Status": "Pending"}}}}}), nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Millisecond)
	defer cancel()
	if _, e := d.wait(ctx, "p", "d", ""); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatalf("%v", e)
	}
}

// 验证匿名部署创建后立即返回的结果
func TestAnonymousNoWait(t *testing.T) {
	o := testOptions(t)
	o.Token = ""
	o.Anonymous = true
	o.NoWait = true
	o.Path = writeFixture(t)
	d := newDeployer(o, io.Discard)
	d.uploadArtifact = func(context.Context, string, object, string) error { return nil }
	d.http.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "" {
			t.Fatal("anonymous auth header")
		}
		switch r.URL.Path {
		case "/pages-public/describe_temp_token":
			return respond(object{"code": 0, "data": object{"Token": "anon-secret", "Expired": 123, "Response": object{"TargetPath": "temp", "ProjectName": "generated"}}}), nil
		case "/pages-public/create_deployment":
			var b object
			json.NewDecoder(r.Body).Decode(&b)
			if str(b, "Token") != "anon-secret" {
				t.Fatal("missing anonymous token")
			}
			return respond(object{"code": 0, "data": object{"Response": object{"ProjectId": "p", "DeploymentId": "d"}}}), nil
		}
		t.Fatal(r.URL.Path)
		return nil, nil
	})
	r, e := d.deploy(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	if r["status"] != "created" || !strings.Contains(str(r, "claimUrl"), "anon-secret") {
		t.Fatal(r)
	}
}

// 验证执行失败时的 JSON 输出
func TestErrorJSON(t *testing.T) {
	t.Setenv("EDGEONE_PAGES_API_TOKEN", "")
	t.Setenv("EDGEONE_MAKER_API_TOKEN", "")
	var out, logs bytes.Buffer
	if code := run([]string{"--json", "--token", "secret", "/does/not/exist"}, &out, &logs); code != 1 {
		t.Fatal(code)
	}
	var r object
	if json.Unmarshal(out.Bytes(), &r) != nil || r["status"] != "error" || strings.Contains(out.String(), "secret") {
		t.Fatal(out.String())
	}
}

// 验证顶层与嵌套 API 业务错误
func TestAPIErrorEnvelopes(t *testing.T) {
	for _, v := range []object{{"Code": 1, "Message": "denied"}, {"Code": 0, "Data": object{"Response": object{"Error": object{"Code": "Unauthorized", "Message": "denied"}}}}} {
		if _, e := unwrap(v, false); e == nil {
			t.Fatal("accepted failure")
		}
	}
}

// 验证临时凭证签名和单文件上传内容
func TestCOSUploadUsesTemporarySignedCredentials(t *testing.T) {
	o := testOptions(t)
	d := newDeployer(o, io.Discard)
	file := filepath.Join(t.TempDir(), "site.zip")
	os.WriteFile(file, []byte("zip fixture"), 0600)
	calls := 0
	d.cosTransport = roundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != "PUT" || r.URL.Host != "bucket-123.cos.accelerate.myqcloud.com" || r.URL.Path != "/temp/site.zip" {
			t.Fatal(r.Method, r.URL)
		}
		if !strings.Contains(r.Header.Get("Authorization"), "q-signature=") || r.Header.Get("x-cos-security-token") != "temporary-token" {
			t.Fatal("temporary credential signature missing")
		}
		b, _ := io.ReadAll(r.Body)
		if string(b) != "zip fixture" {
			t.Fatal(string(b))
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Etag": []string{"fixture"}, "X-Cos-Hash-Crc64ecma": []string{strconv.FormatUint(crc64.Checksum(b, crc64.MakeTable(crc64.ECMA)), 10)}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})
	creds := object{"Bucket": "bucket-123", "Region": "ap-guangzhou", "Credentials": object{"TmpSecretId": "sid", "TmpSecretKey": "key", "Token": "temporary-token"}}
	if e := d.upload(context.Background(), file, creds, "temp/site.zip"); e != nil {
		t.Fatal(e)
	}
	if calls != 1 {
		t.Fatal(calls)
	}
	if e := d.upload(context.Background(), file, object{}, "x"); e == nil {
		t.Fatal("accepted incomplete credentials")
	}
}

// 验证中国站访问地址的凭证参数
func TestSignedVisitURL(t *testing.T) {
	d := newDeployer(testOptions(t), io.Discard)
	d.site = "china"
	d.base = endpoint("china")
	d.http.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		var b object
		json.NewDecoder(r.Body).Decode(&b)
		v := object{}
		switch str(b, "Action") {
		case "DescribePagesProjects":
			v = object{"Projects": []object{{"PresetDomain": "site.example", "Area": "global", "IsTld": 0}}}
		case "DescribePagesEncipherToken":
			if str(b, "Text") != "site.example" {
				t.Fatal(b)
			}
			v = object{"Token": "visit", "Timestamp": 123}
		default:
			t.Fatal(b)
		}
		return respond(object{"Code": 0, "Data": object{"Response": v}}), nil
	})
	u, e := d.visitURL(context.Background(), "p", object{})
	if e != nil || u != "https://site.example?eo_time=123&eo_token=visit" {
		t.Fatal(u, e)
	}
}

// 验证 Agent 配置读取和 .edgeone 归档布局
func TestAIConfigAndEdgeoneLayout(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, ".edgeone")
	os.MkdirAll(filepath.Join(p, "cloud-functions", "node_modules"), 0700)
	os.WriteFile(filepath.Join(p, "cloud-functions", "node_modules", "dependency.js"), []byte("ok"), 0600)
	os.WriteFile(filepath.Join(root, "edgeone.json"), []byte(`{"agents":{"framework":"langgraph"}}`), 0600)
	o := testOptions(t)
	o.Path = p
	d := newDeployer(o, io.Discard)
	file, clean, e := d.prepare(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	defer clean()
	fw, e := agentFramework(file)
	if fw != "langgraph" || e != nil {
		t.Fatal(fw, e)
	}
	r, e := zip.OpenReader(file)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	if len(r.File) != 2 || !strings.Contains(r.File[0].Name, "/.edgeone/cloud-functions/node_modules/") {
		t.Fatal(r.File)
	}
}

// 验证项目创建与直接上传类型检查
func TestCreateProjectAndProviderValidation(t *testing.T) {
	d := newDeployer(testOptions(t), io.Discard)
	d.base = endpoint("china")
	calls := 0
	d.http.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		var b object
		json.NewDecoder(r.Body).Decode(&b)
		calls++
		v := object{"Projects": []object{}}
		if str(b, "Action") == "CreatePagesProject" {
			if str(b, "Area") != "global" || str(b, "Provider") != "Upload" {
				t.Fatal(b)
			}
			v = object{"ProjectId": "new"}
		}
		return respond(object{"Code": 0, "Data": object{"Response": v}}), nil
	})
	p, e := d.project(context.Background())
	if e != nil || str(p, "ProjectId") != "new" || calls != 2 {
		t.Fatal(p, e, calls)
	}
	d.http.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		return respond(object{"Code": 0, "Data": object{"Response": object{"Projects": []object{{"Provider": "GitHub", "ProjectId": "p"}}}}}), nil
	})
	if _, e = d.project(context.Background()); e == nil {
		t.Fatal("accepted git provider")
	}
}

// 验证并发分片上传及合并后的文件内容
func TestCOSMultipartUpload(t *testing.T) {
	o := testOptions(t)
	o.PartSize = 1
	o.Concurrency = 3
	d := newDeployer(o, io.Discard)
	payload := bytes.Repeat([]byte("upload-test"), 320000)
	f := filepath.Join(t.TempDir(), "large.zip")
	os.WriteFile(f, payload, 0600)
	var mu sync.Mutex
	parts := map[string][]byte{}
	d.cosTransport = roundTrip(func(r *http.Request) (*http.Response, error) {
		q := r.URL.Query()
		body := ""
		headers := make(http.Header)
		headers.Set("ETag", "fixture")
		switch {
		case r.Method == "POST" && q.Has("uploads"):
			body = `<InitiateMultipartUploadResult><Bucket>bucket-123</Bucket><Key>temp/site.zip</Key><UploadId>test-upload</UploadId></InitiateMultipartUploadResult>`
		case r.Method == "PUT" && q.Get("partNumber") != "":
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			parts[q.Get("partNumber")] = b
			mu.Unlock()
			headers.Set("x-cos-hash-crc64ecma", strconv.FormatUint(crc64.Checksum(b, crc64.MakeTable(crc64.ECMA)), 10))
		case r.Method == "POST" && q.Get("uploadId") == "test-upload":
			body = `<CompleteMultipartUploadResult><Key>temp/site.zip</Key><ETag>fixture</ETag></CompleteMultipartUploadResult>`
			headers.Set("x-cos-hash-crc64ecma", strconv.FormatUint(crc64.Checksum(payload, crc64.MakeTable(crc64.ECMA)), 10))
		default:
			t.Errorf("unexpected COS request: %s %s", r.Method, r.URL)
		}
		return &http.Response{StatusCode: 200, Header: headers, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	c := object{"Bucket": "bucket-123", "Region": "ap-guangzhou", "Credentials": object{"TmpSecretId": "sid", "TmpSecretKey": "key", "Token": "temporary-token"}}
	if e := d.upload(context.Background(), f, c, "temp/site.zip"); e != nil {
		t.Fatal(e)
	}
	var actual []byte
	for i := 1; i <= len(parts); i++ {
		actual = append(actual, parts[strconv.Itoa(i)]...)
	}
	if len(parts) < 2 || !bytes.Equal(actual, payload) {
		t.Fatal("multipart content mismatch")
	}
}

// 验证 AI Gateway 绑定、跳过绑定和立即返回
func TestAgentBindingAndNoWait(t *testing.T) {
	o := testOptions(t)
	o.NoWait = true
	o.Path = writeFixture(t)
	os.WriteFile(filepath.Join(o.Path, "edgeone.json"), []byte(`{"agents":{"framework":"langgraph"}}`), 0600)
	d := newDeployer(o, io.Discard)
	d.uploadArtifact = func(context.Context, string, object, string) error { return nil }
	bound := false
	d.http.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		var b object
		json.NewDecoder(r.Body).Decode(&b)
		v := object{}
		switch str(b, "Action") {
		case "DescribePagesProjects":
			v = object{"Projects": []object{{"ProjectId": "p", "Provider": "Upload", "ZoneId": "zone"}}}
		case "CreatePagesAgentGatewayCredential":
			if str(b, "ZoneId") != "zone" || str(b, "AgentFramework") != "langgraph" || str(b, "Region") != "ap-guangzhou" {
				t.Fatal(b)
			}
			bound = true
		case "DescribePagesCosTempToken":
			v = object{"TargetPath": "temp"}
		case "CreatePagesDeployment":
			v = object{"DeploymentId": "d"}
		default:
			t.Fatal(b)
		}
		return respond(object{"Code": 0, "Data": object{"Response": v}}), nil
	})
	r, e := d.deploy(context.Background())
	if e != nil || r["status"] != "created" || !bound {
		t.Fatal(r, e, bound)
	}
	o.SkipAI = true
	d.o = o
	bound = false
	if _, e = d.deploy(context.Background()); e != nil || bound {
		t.Fatal(e, bound)
	}
}

// 验证匿名部署完成和失败终态
func TestAnonymousWaitAndTerminalFailure(t *testing.T) {
	d := newDeployer(testOptions(t), io.Discard)
	d.base = endpoint("china")
	d.http.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		return respond(object{"code": 0, "data": object{"Response": object{"Deployment": object{"Status": "Success", "ProjectUrl": "https://anon.example"}}}}), nil
	})
	s, e := d.wait(context.Background(), "p", "d", "anon")
	if e != nil || str(s, "ProjectUrl") != "https://anon.example" {
		t.Fatal(s, e)
	}
	d.http.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		return respond(object{"Code": 0, "Data": object{"Response": object{"Deployments": []object{{"DeploymentId": "d", "Status": "Failed", "Code": -1}}}}}), nil
	})
	if _, e = d.wait(context.Background(), "p", "d", ""); e == nil {
		t.Fatal("accepted failed deployment")
	}
}

// 验证分组帮助、Token 隐藏和 Shell 补全
func TestCobraHelpAndCompletion(t *testing.T) {
	t.Setenv("EDGEONE_PAGES_API_TOKEN", "secret-help-token")
	for _, args := range [][]string{{"--help"}, {"deploy", "--help"}, {"makers", "deploy", "--help"}, {"help", "deploy"}} {
		var out, logs bytes.Buffer
		if code := run(args, &out, &logs); code != 0 {
			t.Fatalf("%v: exit %d: %s", args, code, logs.String())
		}
		text := out.String()
		for _, heading := range []string{"Project & authentication:", "Deployment:", "Upload tuning:", "Waiting & output:"} {
			if !strings.Contains(text, heading) {
				t.Fatalf("%v missing %q: %s", args, heading, text)
			}
		}
		if strings.Contains(text, "secret-help-token") || strings.Contains(text, "--n string") || logs.Len() != 0 {
			t.Fatalf("unclean help: %s %s", text, logs.String())
		}
	}
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		var out, logs bytes.Buffer
		if code := run([]string{"completion", shell}, &out, &logs); code != 0 || out.Len() == 0 {
			t.Fatalf("%s completion: %d %s", shell, code, logs.String())
		}
	}
}

// 验证兼容命令和路径前后的参数解析
func TestCobraLegacyAndInterspersedFlags(t *testing.T) {
	for _, args := range [][]string{
		{"dist", "--n", "example", "--t", "token"},
		{"pages", "deploy", "--name", "example", "dist", "--token", "token"},
		{"--token", "token", "deploy", "dist", "-nexample"},
		{"deploy", "-n", "example", "--token=token", "--", "-dist"},
	} {
		o, e := parse(args)
		if e != nil {
			t.Fatalf("%v: %v", args, e)
		}
		if o.Name != "example" || o.Token != "token" {
			t.Fatalf("bad options %+v", o)
		}
	}
}

// 验证布尔参数和参数错误的 JSON 输出
func TestCobraBooleanParsingAndValidation(t *testing.T) {
	o, e := parse([]string{"deploy", "dist", "--token", "x", "--anonymous=false", "--json=false", "--accelerate=false"})
	if e != nil || o.Anonymous || o.JSON || o.Accelerate {
		t.Fatal(o, e)
	}
	var out, logs bytes.Buffer
	code := run([]string{"deploy", "dist", "--json", "--token", "x", "--env", "invalid"}, &out, &logs)
	var result object
	if code != 2 || json.Unmarshal(out.Bytes(), &result) != nil || result["status"] != "error" || logs.Len() != 0 {
		t.Fatal(code, out.String(), logs.String())
	}
}

var errHelp = errors.New("help displayed")

// 使用正式命令树解析测试参数，不执行部署
func parse(args []string) (options, error) {
	var selected options
	invoked := false
	cmd, current := newCommand(io.Discard, io.Discard, func(o options) error { selected = o; invoked = true; return nil })
	cmd.SetArgs(args)
	err := cmd.Execute()
	if err != nil {
		if current != nil {
			return *current, err
		}
		return selected, err
	}
	if !invoked {
		return selected, errHelp
	}
	return selected, nil
}
