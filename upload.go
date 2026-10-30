package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"

	"github.com/tencentyun/cos-go-sdk-v5"
)

// COS 存储桶和区域名称的校验规则
var cosLabel = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]*$`)

// 使用临时凭证将产物上传到 COS
func (d *deployer) upload(ctx context.Context, file string, creds object, key string) error {
	// 校验临时凭证和上传目标，并登记脱敏字段
	c := obj(creds, "Credentials")
	bucket, region := str(creds, "Bucket"), str(creds, "Region")
	for _, k := range []string{"TmpSecretId", "TmpSecretKey", "Token"} {
		if str(c, k) == "" {
			return errors.New("incomplete COS credentials")
		}
		d.secrets = append(d.secrets, str(c, k))
	}
	if !cosLabel.MatchString(bucket) || !cosLabel.MatchString(region) || key == "" {
		return errors.New("invalid COS bucket, region or key")
	}

	// 选择区域域名或传输加速域名
	host := fmt.Sprintf("%s.cos.%s.myqcloud.com", bucket, region)
	if d.o.Accelerate {
		host = bucket + ".cos.accelerate.myqcloud.com"
	}
	u, _ := url.Parse("https://" + host)

	// 配置连接池、响应期限和签名客户端
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = d.o.Concurrency
	transport.ResponseHeaderTimeout = d.o.RequestTimeout
	var baseTransport http.RoundTripper = transport
	if d.cosTransport != nil {
		baseTransport = d.cosTransport
	}
	client := cos.NewClient(&cos.BaseURL{BucketURL: u}, &http.Client{Transport: &cos.AuthorizationTransport{SecretID: str(c, "TmpSecretId"), SecretKey: str(c, "TmpSecretKey"), SessionToken: str(c, "Token"), Transport: baseTransport}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }})
	defer transport.CloseIdleConnections()

	// 按配置的分片大小和并发数上传文件
	_, _, err := client.Object.Upload(ctx, key, file, &cos.MultiUploadOptions{PartSize: d.o.PartSize, ThreadPoolSize: d.o.Concurrency, CheckPoint: false})
	if err != nil {
		return fmt.Errorf("COS upload failed: %s", d.redact(err.Error()))
	}
	return nil
}

// 调用注入的上传函数或默认 COS 实现
func (d *deployer) doUpload(ctx context.Context, file string, c object, key string) error {
	if d.uploadArtifact != nil {
		return d.uploadArtifact(ctx, file, c, key)
	}
	return d.upload(ctx, file, c, key)
}
