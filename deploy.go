package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"strings"
)

// 生成产物名称使用的随机标识
func randomID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// 查找直接上传项目，或按名称创建新项目
func (d *deployer) project(ctx context.Context) (object, error) {
	// 未指定项目时读取本地关联信息
	if d.o.Name == "" && d.o.ProjectID == "" {
		var p struct{ Name, ProjectId string }
		if b, e := os.ReadFile(".edgeone/project.json"); e == nil {
			if json.Unmarshal(b, &p) == nil {
				d.o.Name = p.Name
				d.o.ProjectID = p.ProjectId
			}
		}
	}
	if d.o.Name == "" && d.o.ProjectID == "" {
		return nil, errors.New("provide --name or --project-id (or .edgeone/project.json)")
	}

	// 按项目 ID 或名称查询项目
	field, value := "Name", d.o.Name
	if d.o.ProjectID != "" {
		field, value = "ProjectId", d.o.ProjectID
	}
	r, err := d.api(ctx, "DescribePagesProjects", object{"Filters": []object{{"Name": field, "Values": []string{value}}}, "Offset": 0, "Limit": 10})
	if err != nil {
		return nil, err
	}

	// 检查已有项目是否支持直接上传
	projects := records(r, "Projects")
	if len(projects) > 0 {
		p := projects[0]
		if str(p, "Provider") != "Upload" {
			return nil, errors.New("project does not use direct upload provider")
		}
		if str(p, "ProjectId") == "" {
			return nil, errors.New("missing project ID")
		}
		return p, nil
	}
	if d.o.ProjectID != "" {
		return nil, errors.New("project ID not found")
	}

	// 名称未匹配到项目时创建新项目
	p, err := d.api(ctx, "CreatePagesProject", object{"Name": d.o.Name, "Provider": "Upload", "Channel": "Custom", "Area": d.o.Area, "Source": "cli"})
	if err != nil {
		return nil, err
	}
	if str(p, "ProjectId") == "" {
		return nil, errors.New("create project returned no ID")
	}
	p["Name"] = d.o.Name
	return p, nil
}

// 执行产物准备、上传和部署流程
func (d *deployer) deploy(ctx context.Context) (object, error) {
	// 准备 ZIP 产物并登记临时文件清理
	done := d.stage("package")
	file, cleanup, err := d.prepare(ctx)
	done()
	if err != nil {
		return nil, err
	}
	defer cleanup()

	// 确定站点，并分流匿名部署
	done = d.stage("resolve-site")
	err = d.resolveSite(ctx)
	done()
	if err != nil {
		return nil, err
	}
	if d.o.Anonymous && d.o.Token == "" {
		return d.anonymous(ctx, file)
	}

	// 查找或创建目标项目
	done = d.stage("resolve-project")
	p, err := d.project(ctx)
	done()
	if err != nil {
		return nil, err
	}
	id := str(p, "ProjectId")

	// 预览部署先检查正式环境部署记录
	if d.o.Env == "preview" {
		r, e := d.api(ctx, "DescribePagesDeployments", object{"ProjectId": id, "Offset": 0, "Limit": 50, "Order": "Desc", "OrderBy": "CreatedOn"})
		if e != nil {
			return nil, e
		}
		found := false
		for _, v := range records(r, "Deployments") {
			if v["UsedInProd"] == true {
				found = true
			}
		}
		if !found {
			return nil, errors.New("preview requires an existing production deployment")
		}
	}

	// 按产物配置绑定 AI Gateway 云凭证
	if !d.o.SkipAI {
		framework, e := agentFramework(file)
		if e != nil {
			return nil, e
		}
		if framework != "" {
			zone := str(p, "ZoneId")
			if zone == "" {
				detail, e := d.api(ctx, "DescribePagesProjects", object{"Filters": []object{{"Name": "ProjectId", "Values": []string{id}}}, "Offset": 0, "Limit": 1})
				if e != nil {
					return nil, e
				}
				ps := records(detail, "Projects")
				if len(ps) > 0 {
					zone = str(ps[0], "ZoneId")
				}
			}
			if zone == "" {
				return nil, errors.New("AI Gateway binding requires project ZoneId; use --skip-ai-gateway-sync to skip")
			}
			region := "ap-guangzhou"
			if d.site == "global" {
				region = "ap-singapore"
			}
			if _, e = d.api(ctx, "CreatePagesAgentGatewayCredential", object{"ProjectId": id, "ZoneId": zone, "AgentFramework": framework, "Region": region}); e != nil {
				return nil, e
			}
		}
	}

	// 获取 COS 临时上传凭证和目标路径
	done = d.stage("credentials")
	creds, err := d.api(ctx, "DescribePagesCosTempToken", object{"ProjectId": id})
	done()
	if err != nil {
		return nil, err
	}
	target := str(creds, "TargetPath")
	if target == "" {
		return nil, errors.New("missing COS target path")
	}
	key := path.Join(target, "artifact-"+randomID()+".zip")

	// 上传 ZIP 产物
	done = d.stage("upload")
	err = d.doUpload(ctx, file, creds, key)
	done()
	if err != nil {
		return nil, err
	}

	// 转换部署环境并创建部署记录
	env := "Production"
	if d.o.Env == "preview" {
		env = "Preview"
	}
	done = d.stage("create-deployment")
	r, err := d.api(ctx, "CreatePagesDeployment", object{"ProjectId": id, "ViaMeta": "Upload", "Provider": "Upload", "Env": env, "DistType": "Zip", "TempBucketPath": key, "BuildFrom": "CLI"})
	done()
	if err != nil {
		return nil, err
	}
	deployment := str(r, "DeploymentId")
	if deployment == "" {
		return nil, errors.New("missing deployment ID")
	}

	// 生成控制台地址并处理立即返回选项
	console := "https://console.tencentcloud.com"
	if d.site == "china" {
		console = "https://console.cloud.tencent.com"
	}
	result := object{"status": "created", "projectId": id, "deploymentId": deployment, "env": env, "consoleUrl": console + "/edgeone/pages/project/" + id + "/deployment/" + deployment}
	if d.o.NoWait {
		return result, nil
	}

	// 等待发布完成并获取访问地址
	done = d.stage("wait")
	state, err := d.wait(ctx, id, deployment, "")
	done()
	if err != nil {
		return nil, err
	}
	result["status"] = "success"
	done = d.stage("resolve-url")
	u, err := d.visitURL(ctx, id, state)
	done()
	if err != nil {
		return nil, err
	}
	result["url"] = u
	return result, nil
}

// 轮询部署状态，处理终态、连续失败和超时
func (d *deployer) wait(ctx context.Context, id, deployment, anonymousToken string) (object, error) {
	previous := ""
	failures := 0
	for {
		var state object
		var err error

		// 通过对应接口查询当前部署状态
		if anonymousToken != "" {
			_, r, e := d.public(ctx, "describe_deployment", object{"Token": anonymousToken, "ProjectId": id, "DeploymentIds": []string{deployment}, "NeedVisit": true})
			err = e
			state = obj(r, "Deployment")
		} else {
			var r object
			r, err = d.api(ctx, "DescribePagesDeployments", object{"ProjectId": id, "DeploymentIds": []string{deployment}, "Offset": 0, "Limit": 50, "OrderBy": "CreatedOn", "Order": "Desc"})
			for _, v := range records(r, "Deployments") {
				if str(v, "DeploymentId") == deployment {
					state = v
					break
				}
			}
		}

		// 累计连续查询失败，并处理部署状态变化
		if err != nil {
			failures++
			if failures >= 3 {
				return nil, fmt.Errorf("deployment %s: polling failed three times: %w", deployment, err)
			}
			d.log("Polling request failed (%d/3): %v", failures, err)
		} else {
			failures = 0
			if state == nil {
				return nil, fmt.Errorf("deployment %s not found in API response", deployment)
			}
			status := str(state, "Status")
			if status != previous {
				d.log("Deployment %s: %s", deployment, status)
				previous = status
			}
			switch status {
			case "Success":
				return state, nil
			case "Pending", "Process":
			case "Failed", "Timeout", "Cancelled", "Invalid":
				return nil, fmt.Errorf("deployment %s ended as %s (code %v)", deployment, status, state["Code"])
			default:
				return nil, fmt.Errorf("unknown deployment status %q", status)
			}
		}

		// 等待下一次查询或响应取消信号
		if err := pause(ctx, d.o.Poll); err != nil {
			return nil, fmt.Errorf("waiting for deployment %s: %w (remote deployment may continue)", deployment, err)
		}
	}
}

// 获取部署访问地址并按需附加访问凭证
func (d *deployer) visitURL(ctx context.Context, id string, state object) (string, error) {
	// 查询项目域名和访问配置
	r, err := d.api(ctx, "DescribePagesProjects", object{"Filters": []object{{"Name": "ProjectId", "Values": []string{id}}}, "Offset": 0, "Limit": 1})
	if err != nil {
		return "", err
	}
	ps := records(r, "Projects")
	if len(ps) == 0 {
		return "", errors.New("project missing after deployment")
	}
	p := ps[0]

	// 正式部署优先使用已通过审核的自定义域名
	if d.o.Env == "production" {
		for _, v := range records(p, "CustomDomains") {
			if str(v, "Status") == "Pass" {
				return "https://" + str(v, "Domain"), nil
			}
		}
	}

	// 选择正式环境预设域名或预览域名
	domain := str(p, "PresetDomain")
	if d.o.Env == "preview" && str(state, "PreviewUrl") != "" {
		domain = str(state, "PreviewUrl")
	}
	if domain == "" {
		return "", errors.New("deployment succeeded but no domain returned")
	}
	u, err := url.Parse("https://" + strings.TrimPrefix(domain, "https://"))
	if err != nil {
		return "", err
	}
	// 为需要鉴权的中国站预设域名附加访问凭证
	if d.site == "china" && str(p, "Area") != "overseas" && fmt.Sprint(p["IsTld"]) != "1" {
		r, err = d.api(ctx, "DescribePagesEncipherToken", object{"Text": u.Host})
		if err != nil {
			return "", fmt.Errorf("deployment succeeded but access token failed: %w", err)
		}
		if str(r, "Token") == "" || r["Timestamp"] == nil {
			return "", errors.New("deployment succeeded but access-token response is incomplete")
		}
		q := u.Query()
		q.Set("eo_token", str(r, "Token"))
		q.Set("eo_time", fmt.Sprint(r["Timestamp"]))
		u.RawQuery = q.Encode()
	}
	return u.String(), nil
}

// 使用匿名凭证上传产物并创建临时部署
func (d *deployer) anonymous(ctx context.Context, file string) (object, error) {

	// 申请匿名部署 Token 和 COS 临时凭证
	done := d.stage("credentials")
	data, c, err := d.public(ctx, "describe_temp_token", object{"Language": map[bool]string{true: "zh-CN", false: "en"}[d.site == "china"], "ProjectName": "deploy-" + randomID()})
	done()
	if err != nil {
		return nil, err
	}
	token := str(data, "Token")
	if token == "" {
		return nil, errors.New("anonymous API returned no token")
	}
	d.secrets = append(d.secrets, token)

	// 检查匿名上传路径并生成产物名称
	key := path.Join(str(c, "TargetPath"), "artifact-"+randomID()+".zip")
	if str(c, "TargetPath") == "" {
		return nil, errors.New("missing anonymous COS target path")
	}

	// 上传匿名部署产物
	done = d.stage("upload")
	err = d.doUpload(ctx, file, c, key)
	done()
	if err != nil {
		return nil, err
	}

	// 创建匿名部署并取得项目与部署标识
	done = d.stage("create-deployment")
	_, r, err := d.public(ctx, "create_deployment", object{"Token": token, "ProjectName": str(c, "ProjectName"), "TempBucketPath": key, "DistType": "Zip"})
	done()
	if err != nil {
		return nil, err
	}
	id, deployment := str(r, "ProjectId"), str(r, "DeploymentId")
	if id == "" || deployment == "" {
		return nil, errors.New("anonymous deployment returned no IDs")
	}
	console := "https://console.tencentcloud.com"
	if d.site == "china" {
		console = "https://console.cloud.tencent.com"
	}

	// 生成认领地址并处理立即返回选项
	result := object{"status": "created", "projectId": id, "deploymentId": deployment, "anonymousToken": token, "claimUrl": console + "/edgeone/makers/claim?token=" + url.QueryEscape(token), "expiresAt": data["Expired"], "site": d.site}
	if d.o.NoWait {
		return result, nil
	}

	// 等待匿名部署完成并返回站点地址
	done = d.stage("wait")
	state, err := d.wait(ctx, id, deployment, token)
	done()
	if err != nil {
		return nil, err
	}
	result["status"] = "success"
	result["url"] = str(state, "ProjectUrl")
	return result, nil
}
