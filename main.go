package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
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
	Attempts int
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

	client := NewClient(clientID, clientSecret, offlineToken)

	// Verify we can get a token
	if err := client.VerifyToken(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to get access token: %v\n", err)
		os.Exit(1)
	}

	// Set up signal handling for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	var interrupted bool
	go func() {
		<-sigCh
		fmt.Fprintln(os.Stderr, "received shutdown signal, cleaning up...")
		interrupted = true
		cancel()
	}()

	jobs := expandMatrix(config.Matrix)
	results := runBuilds(ctx, client, config, jobs)

	if interrupted {
		os.Exit(1)
	}

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

func runBuilds(ctx context.Context, client *Client, config *Config, jobs []BuildJob) []BuildResult {
	var wg sync.WaitGroup
	results := make([]BuildResult, len(jobs))

	for i, job := range jobs {
		wg.Add(1)
		go func(idx int, j BuildJob) {
			defer wg.Done()
			results[idx] = runSingleBuild(ctx, client, config, j)
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

func runSingleBuild(ctx context.Context, client *Client, config *Config, job BuildJob) BuildResult {
	start := time.Now()

	apiImageType, uploadType := mapImageType(job.ImageType)

	uploadOptions := map[string]any{}
	if job.ImageType == "aws" {
		uploadOptions["share_with_accounts"] = []string{config.AWSAccountID}
	}

	const maxAttempts = 3
	var lastErr error

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Check for cancellation before starting attempt
		select {
		case <-ctx.Done():
			return BuildResult{
				Job:      job,
				Success:  false,
				Duration: time.Since(start),
				Attempts: attempt,
				Error:    "interrupted",
			}
		default:
		}

		if attempt > 1 {
			job.Log("retrying (attempt %d/%d)", attempt, maxAttempts)
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

		blueprintID, err := client.CreateBlueprint(blueprintReq)
		if err != nil {
			lastErr = fmt.Errorf("create blueprint: %s", err.Error())
			job.Log("attempt %d failed: %v", attempt, lastErr)
			continue
		}

		// Step 2: Compose from the blueprint
		composeID, err := client.ComposeFromBlueprint(blueprintID)
		if err != nil {
			_ = client.DeleteBlueprint(blueprintID)
			lastErr = fmt.Errorf("compose from blueprint: %s", err.Error())
			job.Log("attempt %d failed: %v", attempt, lastErr)
			continue
		}

		job.Log("compose %s created", composeID)

		// Step 3: Wait for compose to complete
		success, err := client.WaitForCompose(ctx, composeID)
		_ = client.DeleteBlueprint(blueprintID)

		// If cancelled, return immediately
		if ctx.Err() != nil {
			job.Log("interrupted, blueprint deleted")
			return BuildResult{
				Job:      job,
				Success:  false,
				Duration: time.Since(start),
				Attempts: attempt,
				Error:    "interrupted",
			}
		}

		if success {
			job.Log("compose %s succeeded", composeID)
			return BuildResult{
				Job:      job,
				Success:  true,
				Duration: time.Since(start),
				Attempts: attempt,
			}
		}

		if err != nil {
			lastErr = err
			job.Log("compose %s failed: %s", composeID, err.Error())
		} else {
			lastErr = fmt.Errorf("compose failed with unknown error")
		}
	}

	// All attempts failed
	return BuildResult{
		Job:      job,
		Success:  false,
		Duration: time.Since(start),
		Attempts: maxAttempts,
		Error:    lastErr.Error(),
	}
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
		if r.Attempts > 1 {
			line += fmt.Sprintf(" [%d attempts]", r.Attempts)
		}
		if r.Error != "" {
			errMsg := r.Error
			if len(errMsg) > 50 {
				errMsg = errMsg[:50] + "..."
			}
			line += fmt.Sprintf(" (%s)", errMsg)
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
