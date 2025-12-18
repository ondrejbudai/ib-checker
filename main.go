package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-yaml"
)

type Config struct {
	Matrix       Matrix `yaml:"matrix"`
	AWSAccountID string `yaml:"aws_account_id"`
}

type Matrix struct {
	Distributions []string `yaml:"distributions"`
	ImageTypes    []string `yaml:"image_types"`
}

type BuildJob struct {
	Distribution string
	ImageType    string
}

func (j BuildJob) Log(format string, args ...any) {
	timestamp := time.Now().Format("2006-01-02 15:04:05")
	prefix := fmt.Sprintf("%s [%s/%s] ", timestamp, j.Distribution, j.ImageType)
	fmt.Fprintf(os.Stderr, prefix+format+"\n", args...)
}

type BuildResult struct {
	Job      BuildJob
	Success  bool
	Duration time.Duration
	Error    string
}

func main() {
	flag.Parse()
	args := flag.Args()
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "Usage: ib-checker <config-file>")
		os.Exit(1)
	}
	configPath := args[0]

	config, err := loadConfig(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}

	offlineToken := os.Getenv("OFFLINE_TOKEN")
	clientID := os.Getenv("CLIENT_ID")
	clientSecret := os.Getenv("CLIENT_SECRET")

	if offlineToken == "" && (clientID == "" || clientSecret == "") {
		fmt.Fprintln(os.Stderr, "OFFLINE_TOKEN or CLIENT_ID+CLIENT_SECRET environment variables are required")
		os.Exit(1)
	}

	tokenManager := NewTokenManager(clientID, clientSecret, offlineToken)

	// Verify we can get a token
	if _, err := tokenManager.GetToken(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to get access token: %v\n", err)
		os.Exit(1)
	}

	jobs := expandMatrix(config.Matrix)
	results := runBuilds(tokenManager, config, jobs)

	message := formatResults(results)
	fmt.Println(message)

	messagePrefix := os.Getenv("MESSAGE_PREFIX")
	sendNotifications(messagePrefix, message)

	for _, r := range results {
		if !r.Success {
			os.Exit(1)
		}
	}
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, err
	}

	return &config, nil
}

func expandMatrix(matrix Matrix) []BuildJob {
	var jobs []BuildJob
	for _, dist := range matrix.Distributions {
		for _, imgType := range matrix.ImageTypes {
			jobs = append(jobs, BuildJob{
				Distribution: dist,
				ImageType:    imgType,
			})
		}
	}
	return jobs
}

func runBuilds(tokenManager *TokenManager, config *Config, jobs []BuildJob) []BuildResult {
	var wg sync.WaitGroup
	results := make([]BuildResult, len(jobs))

	for i, job := range jobs {
		wg.Add(1)
		go func(idx int, j BuildJob) {
			defer wg.Done()
			results[idx] = runSingleBuild(tokenManager, config, j)
		}(i, job)
	}

	wg.Wait()
	return results
}

func mapImageType(configType string) (apiType string, uploadType string) {
	switch configType {
	case "qcow2":
		return "guest-image", "aws.s3"
	case "aws":
		return "aws", "aws"
	case "image-installer":
		return "image-installer", "aws.s3"
	case "vsphere-ova":
		return "vsphere-ova", "aws.s3"
	case "wsl":
		return "wsl", "aws.s3"
	default:
		return configType, "aws.s3"
	}
}

func runSingleBuild(tokenManager *TokenManager, config *Config, job BuildJob) BuildResult {
	start := time.Now()

	apiImageType, uploadType := mapImageType(job.ImageType)

	uploadOptions := map[string]any{}
	if job.ImageType == "aws" {
		uploadOptions["share_with_accounts"] = []string{config.AWSAccountID}
	}

	// Step 1: Create a blueprint
	blueprintName := fmt.Sprintf("ibc-%s-%s-%d", job.Distribution, job.ImageType, time.Now().UnixNano())
	blueprintReq := CreateBlueprintRequest{
		Name:         blueprintName,
		Description:  "ib-checker temporary blueprint",
		Distribution: job.Distribution,
		ImageRequests: []BlueprintImageRequest{
			{
				Architecture: "x86_64",
				ImageType:    apiImageType,
				UploadRequest: BlueprintUploadRequest{
					Type:    uploadType,
					Options: uploadOptions,
				},
			},
		},
		Customizations: map[string]any{},
	}

	blueprintID, err := createBlueprint(tokenManager, blueprintReq)
	if err != nil {
		return BuildResult{
			Job:      job,
			Success:  false,
			Duration: time.Since(start),
			Error:    fmt.Sprintf("create blueprint: %s", err.Error()),
		}
	}

	// Step 2: Compose from the blueprint
	composeID, err := composeFromBlueprint(tokenManager, blueprintID)
	if err != nil {
		_ = deleteBlueprint(tokenManager, blueprintID)
		return BuildResult{
			Job:      job,
			Success:  false,
			Duration: time.Since(start),
			Error:    fmt.Sprintf("compose from blueprint: %s", err.Error()),
		}
	}

	job.Log("compose %s created", composeID)

	// Step 3: Wait for compose to complete
	success, err := waitForCompose(tokenManager, composeID)
	duration := time.Since(start)

	// Step 4: Delete the blueprint (cleanup)
	_ = deleteBlueprint(tokenManager, blueprintID)

	result := BuildResult{
		Job:      job,
		Success:  success,
		Duration: duration,
	}

	if success {
		job.Log("compose %s succeeded", composeID)
	} else if err != nil {
		result.Error = err.Error()
		job.Log("compose %s failed: %s", composeID, err.Error())
	}

	return result
}

func formatResults(results []BuildResult) string {
	var sb strings.Builder

	for _, r := range results {
		var emoji string
		if r.Success {
			emoji = "✅"
		} else {
			emoji = "❌"
		}

		duration := r.Duration.Round(time.Second).String()
		line := fmt.Sprintf("- %s %s/%s %s", emoji, r.Job.Distribution, r.Job.ImageType, duration)
		if r.Error != "" {
			line += fmt.Sprintf(" (%s)", r.Error)
		}
		sb.WriteString(line + "\n")
	}

	return strings.TrimSpace(sb.String())
}

func sendNotifications(prefix, message string) {
	var wg sync.WaitGroup

	fullMessage := message
	if prefix != "" {
		fullMessage = prefix + "\n" + message
	}

	if webhook := os.Getenv("SLACK_WEBHOOK"); webhook != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sendSlackNotification(webhook, fullMessage)
		}()
	}

	if webhook := os.Getenv("TELEGRAM_WEBHOOK"); webhook != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sendTelegramNotification(webhook, fullMessage)
		}()
	}

	wg.Wait()
}

func sendSlackNotification(webhook, message string) {
	payload := map[string]string{"text": message}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(webhook, "application/json", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to send Slack notification: %v\n", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "Slack notification failed: %s\n", resp.Status)
	}
}

func sendTelegramNotification(webhook, message string) {
	payload := map[string]string{"text": message}
	body, _ := json.Marshal(payload)

	resp, err := http.Post(webhook, "application/json", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to send Telegram notification: %v\n", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "Telegram notification failed: %s\n", resp.Status)
	}
}
