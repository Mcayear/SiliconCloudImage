package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// API 响应结构体
type OriginResponse struct {
	Images  []Image       `json:"images"`
	Timings TimingDetails `json:"timings"` // 分解成独立结构体
	Seed    json.Number   `json:"seed"`    // 处理可能为字符串或数字的字段
}

// 新增 Timing 结构体处理灵活数据类型
type TimingDetails struct {
	Inference json.Number `json:"inference"` // 使用 json.Number 类型
}

// 修改 image 结构体能应对上游字段变化
type Image struct {
	URL           string      `json:"url"`
	RevisedPrompt string      `json:"revised_prompt,omitempty"`
	ExtraFields   interface{} `json:"-"` // 捕获未定义字段
}

type OpenAIResponse struct {
	Created int64            `json:"created"`
	Data    []OpenAIDataItem `json:"data"`
}

type OpenAIDataItem struct {
	B64JSON       string `json:"b64_json"`
	RevisedPrompt string `json:"revised_prompt,omitempty"`
}

// 安全日志标头处理
func safeLogHeaders(headers http.Header) map[string]string {
	safe := make(map[string]string)
	for k, v := range headers {
		if strings.EqualFold(k, "Authorization") && len(v) > 0 {
			safe[k] = fmt.Sprintf("%s...", v[0][:min(10, len(v[0]))])
		} else {
			safe[k] = strings.Join(v, ", ")
		}
	}
	return safe
}

// 新增工具函数读取响应体内容
func readBody(r io.Reader) string {
	buf := new(bytes.Buffer)
	_, err := buf.ReadFrom(r)
	if err != nil {
		return ""
	}
	return buf.String()
}

// 转发处理器
func handleGenerations(w http.ResponseWriter, r *http.Request) {
	targetURL := "https://api.siliconflow.cn/v1/images/generations"
	startTime := time.Now()

	// 记录请求信息
	log.Printf("[REQUEST] %s %s", r.Method, r.URL.Path)
	defer func() {
		log.Printf("[COMPLETE] 总耗时: %v", time.Since(startTime))
	}()

	// 读取并处理请求体
	var reqBody map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
		log.Printf("[ERROR] 请求体内容: %s", readBody(r.Body))
		http.Error(w, `{"error": "Invalid JSON"}`, http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	// 字段映射
	if size, ok := reqBody["size"]; ok {
		reqBody["image_size"] = size
		delete(reqBody, "size")
	}

	// 转发请求
	client := &http.Client{Timeout: 15 * time.Second}
	bodyBytes, _ := json.Marshal(reqBody)
	proxyReq, _ := http.NewRequest(http.MethodPost, targetURL, bytes.NewReader(bodyBytes))

	// 复制标头
	for k, v := range r.Header {
		proxyReq.Header[k] = v
	}

	log.Printf("[FORWARD] 请求体: %s", string(bodyBytes))

	// 发送请求
	resp, err := client.Do(proxyReq)
	if err != nil {
		log.Printf("[ERROR] API请求失败: %v", err)
		http.Error(w, `{"error":"Upstream service unavailable"}`, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	var originResp OriginResponse
	if err := json.NewDecoder(resp.Body).Decode(&originResp); err != nil {
		log.Printf("[ERROR] 原始响应内容: %s", readBody(resp.Body)) // 需要实现 readBody 函数
		log.Printf("[ERROR] 响应解析失败: %v", err)
		http.Error(w, `{"error":"Invalid upstream response"}`, http.StatusInternalServerError)
		return
	}

	// 判断响应格式
	responseFormat, _ := reqBody["response_format"].(string)
	if responseFormat != "b64_json" {
		log.Printf("[SKIP] 直接返回URL格式")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(originResp)
		return
	}

	// 并发下载转换图片
	resultChan := make(chan OpenAIDataItem, len(originResp.Images))
	errorChan := make(chan error, len(originResp.Images))

	downloadImage := func(url string, index int) {
		log.Printf("[DOWNLOAD %d] 开始下载: %s", index, url)
		start := time.Now()

		resp, err := http.Get(url)
		if err != nil {
			log.Printf("[ERROR %d] 下载失败: %v", index, err)
			errorChan <- err
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			err := fmt.Errorf("HTTP %d", resp.StatusCode)
			log.Printf("[ERROR %d] 响应错误: %v", index, err)
			errorChan <- err
			return
		}

		data, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Printf("[ERROR %d] 读取失败: %v", index, err)
			errorChan <- err
			return
		}

		b64 := base64.StdEncoding.EncodeToString(data)
		log.Printf("[SUCCESS %d] 下载完成，大小: %d bytes, 耗时: %v",
			index, len(data), time.Since(start))

		resultChan <- OpenAIDataItem{
			B64JSON:       b64,
			RevisedPrompt: originResp.Images[index].RevisedPrompt,
		}
	}

	for i, img := range originResp.Images {
		go downloadImage(img.URL, i)
	}

	// 收集结果
	results := make([]OpenAIDataItem, 0, len(originResp.Images))
	for range originResp.Images {
		select {
		case res := <-resultChan:
			results = append(results, res)
		case err := <-errorChan:
			log.Printf("[WARN] 部分图片下载失败: %v", err)
			results = append(results, OpenAIDataItem{B64JSON: ""})
		}
	}

	// 构造响应
	openaiResp := OpenAIResponse{
		Created: time.Now().Unix(),
		Data:    results,
	}

	log.Printf("[SUCCESS] 返回数据 - 图片数量: %d", len(results))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(openaiResp)
}

func main() {
	http.HandleFunc("/v1/images/generations", handleGenerations)

	port := ":3000"
	log.Printf("[SERVER] 服务启动在 http://localhost%s", port)
	if err := http.ListenAndServe(port, nil); err != nil {
		log.Fatal("[FATAL] 启动失败: ", err)
	}
}
