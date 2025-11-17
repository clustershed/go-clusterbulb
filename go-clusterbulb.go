// Project: go-clusterbulb
// Author: ClusterShed / Chris Mayenschein
// Version: 0.0.4
//
// Description:
// go-clusterbulb is a Kubernetes cluster health monitoring tool that integrates with
// Home Assistant to visually indicate the health status of the cluster using a smart
// bulb. It checks for node and pod health, warning events, and open GitHub pull requests,
// updating the bulb color accordingly.
//
//	Green: Healthy cluster
//	Blue: Open GitHub pull requests
//	Red: Detected issues in the cluster
//	Blinking Red-Blue: Both open pull requests and detected issues
//
// Usage:
// Deploy go-clusterbulb as a pod within your Kubernetes cluster with the
// necessary environment variables set for Home Assistant and GitHub access.
//
// # Project is setup to run in a base alpine image with gcompat installed
//
// Note: Ensure the pod has the necessary RBAC permissions to read nodes, pods,
// and events in the cluster. This app will not run as root/superuser for security reasons.
//
// Environment Variables:
// - HA_TOKEN: Home Assistant Long-Lived Access Token
// - HA_URL: Base URL of your Home Assistant instance (e.g., http://homeassistant.local:8123)
// - HA_LIGHT_ENTITY_ID: Entity ID of the smart bulb in Home Assistant (e.g., light.cluster_bulb)
// - HA_LIGHT_BRIGHTNESS: Brightness level for the bulb (0-255, default 255)
// - GH_OWNER: GitHub repository owner (user or organization)
// - GH_REPO: GitHub repository name
// - GH_TOKEN: (Optional/Recommended) GitHub Personal Access Token for authenticated API requests
// - GH_PR_CHECK_INTERVAL: Interval in seconds to check for open pull requests (default 300 seconds)
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/user"
	"strconv"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Environment variables
var haToken = ""               // os.Getenv("HA_TOKEN")
var haUrl = ""                 // os.Getenv("HA_URL")
var haLightEntityId = ""       // os.Getenv("HA_LIGHT_ENTITY_ID")
var haLightBrightness = 255    // os.Getenv("HA_LIGHT_BRIGHTNESS") // 0-255
var ghOwner = ""               // os.Getenv("GH_OWNER")
var ghRepo = ""                // os.Getenv("GH_REPO")
var ghToken = ""               // os.Getenv("GH_TOKEN")
var ghPRCheckInterval = 5 * 60 // os.Getenv("GH_PR_CHECK_INTERVAL") // Seconds default:300

// Variables to track known issues, cluster state, and HA bulb color state
var knownIssues = make(map[string]time.Time)
var clusterState = "healthy"
var ghPRState = "none"
var haLastColorState = "healthy"
var pullRequests = []Issue{}

// Issue represents a detected cluster issue
type Issue struct {
	Key       string    `json:"key"`
	Type      string    `json:"type"`
	Message   string    `json:"message"`
	Timestamp time.Time `json:"timestamp"`
}

// HealthReport represents the overall cluster health summary
type HealthReport struct {
	Timestamp    time.Time `json:"timestamp"`
	NodeIssues   []Issue   `json:"node_issues"`
	PodIssues    []Issue   `json:"pod_issues"`
	EventIssues  []Issue   `json:"event_issues"`
	PullRequests []Issue   `json:"pull_requests"`
	TotalIssues  int       `json:"total_issues"`
	ClusterState string    `json:"cluster_state"`
}

// PullRequest represents a GitHub pull request
type PullRequest struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	User      User      `json:"user"`
	State     string    `json:"state"`
	HTMLURL   string    `json:"html_url"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// User represents a GitHub user
type User struct {
	Login string `json:"login"`
}

func main() {

	// Prevent running as root/superuser
	if isSuperUser() {
		log.Fatalf("Running with superuser privileges is not permitted.")
		os.Exit(1)
	}

	// Gather environment variables
	ghOwner = os.Getenv("GH_OWNER")
	ghRepo = os.Getenv("GH_REPO")
	ghToken = os.Getenv("GH_TOKEN") // optional/recommended (GitHub API rate limits apply)
	ghPRCheckIntervalStr := os.Getenv("GH_PR_CHECK_INTERVAL")
	haToken = os.Getenv("HA_TOKEN")
	haUrl = os.Getenv("HA_URL")
	haLightEntityId = os.Getenv("HA_LIGHT_ENTITY_ID")
	haLightBrightnessStr := os.Getenv("HA_LIGHT_BRIGHTNESS")

	// Parse GH_PR_CHECK_INTERVAL (seconds) with a default
	ghPRCheckInterval = 300 // default seconds (5 minutes)
	if ghPRCheckIntervalStr != "" {
		if v, err := strconv.Atoi(ghPRCheckIntervalStr); err == nil && v > 0 {
			ghPRCheckInterval = v
		} else {
			log.Printf("Invalid GH_PR_CHECK_INTERVAL '%s', using default %d", ghPRCheckIntervalStr, ghPRCheckInterval)
			os.Exit(1)
		}
	}
	// Parse HA_LIGHT_BRIGHTNESS and ensure it is a valid integer between 0-255
	haLightBrightness = 255 // default brightness
	if haLightBrightnessStr != "" {
		if v, err := strconv.Atoi(haLightBrightnessStr); err == nil && v > 0 {
			if v > 255 || v < 1 {
				log.Printf("HA_LIGHT_BRIGHTNESS '%s' out of range (1-255), using default %d", haLightBrightnessStr, haLightBrightness)
				os.Exit(1)
			}
			haLightBrightness = v
		} else {
			log.Printf("Invalid HA_LIGHT_BRIGHTNESS '%s', using default %d", haLightBrightnessStr, haLightBrightness)
			os.Exit(1)
		}
	}

	// Setup the tickers
	tickerHABulbUpdate := time.NewTicker(1 * time.Second) // every second for smooth updates to bulb
	tickerClusterChecks := time.NewTicker(10 * time.Second)
	tickerGitHubPRChecks := time.NewTicker(time.Duration(ghPRCheckInterval) * time.Second)

	// Quit channel for clean shutdown
	quit := make(chan struct{})

	// Run tasks concurrently
	go func() {
		for {
			select {
			case <-tickerHABulbUpdate.C:
				haUpdateBulb()
			case <-tickerClusterChecks.C:
				clusterChecks()
			case <-tickerGitHubPRChecks.C:
				ghPullRequestsCheck()
			case <-quit:
				tickerHABulbUpdate.Stop()
				tickerClusterChecks.Stop()
				tickerGitHubPRChecks.Stop()
				fmt.Println("Scheduler stopped.")
				return
			}
		}
	}()

	// Keep the main function running indefinitely
	select {}
}

var errorCount int
var errorLimit = 5 // change as needed
func HandleError(msg string, err error) {
	if err == nil {
		return
	}
	errorCount++
	fmt.Printf("%s %s %v\n", time.Now().Format(time.RFC3339), msg, err)
	if errorCount >= errorLimit {
		fmt.Printf("Error limit (%d) reached. Exiting.\n", errorLimit)
		os.Exit(1)
	}
}

// isSuperUser checks if the current user is root (uid 0)
func isSuperUser() bool {
	// Check effective user ID directly first
	if os.Geteuid() == 0 {
		return true
	}

	// Fallback using os/user
	currentUser, err := user.Current()
	if err != nil {
		return false
	}

	uid, err := strconv.Atoi(currentUser.Uid)
	if err != nil {
		return false
	}

	return uid == 0
}

func haUpdateBulb() {
	// Home Assistant bulb update logic
	switch clusterState {
	case "healthy":
		// Set bulb to green
		haLastColorState = "healthy"
		haSetBulbColors(0, 255, 0)
	case "pull_requests_open":
		// Set bulb to blue
		haLastColorState = "pull_requests_open"
		haSetBulbColors(0, 0, 255)
	case "issues_detected":
		// Set bulb to red
		haLastColorState = "issues_detected"
		haSetBulbColors(255, 0, 0)
	case "pull_requests_open|issues_detected":
		// Set bulb to blinking red-blue
		if haLastColorState == "issues_detected" {
			haLastColorState = "pull_requests_open"
			haSetBulbColors(0, 0, 255)
		} else {
			haLastColorState = "issues_detected"
			haSetBulbColors(255, 0, 0)
		}
	}
}

func haSetBulbColors(colorR int, colorG int, colorB int) {

	// Ensure required environment variables are set otherwise skip
	if haToken == "" || haUrl == "" || haLightEntityId == "" {
		return
	}

	// Prepare payload
	payload := map[string]interface{}{
		"entity_id":  haLightEntityId,
		"rgb_color":  []int{colorR, colorG, colorB},
		"brightness": haLightBrightness,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		fmt.Printf("Error marshaling payload: %v\n", err)
		return
	}

	// Create POST request
	req, err := http.NewRequest("POST", fmt.Sprintf("%s/api/services/light/turn_on", haUrl), bytes.NewBuffer(body))
	if err != nil {
		fmt.Printf("Error creating request: %v\n", err)
		return
	}

	// Set headers
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", haToken))
	req.Header.Set("Content-Type", "application/json")

	// Send request
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("Error sending request: %v\n", err)
		return
	}
	defer resp.Body.Close()
}

func clusterChecks() {
	ctx := context.Background()

	// In-cluster configuration
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("Failed to get in-cluster config: %v", err)
		os.Exit(1)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("Failed to create clientset: %v", err)
		os.Exit(1)
	}

	report := &HealthReport{
		Timestamp: time.Now(),
	}

	nodeIssues := checkNodes(ctx, clientset)
	podIssues := checkPods(ctx, clientset)
	eventIssues := checkEvents(ctx, clientset)
	report.NodeIssues = nodeIssues
	report.PodIssues = podIssues
	report.EventIssues = eventIssues
	report.PullRequests = pullRequests
	report.TotalIssues = len(nodeIssues) + len(podIssues) + len(eventIssues)

	if report.TotalIssues == 0 {
		report.ClusterState = "healthy"
		if ghPRState == "open" {
			report.ClusterState = "pull_requests_open"
		}
	} else {
		report.ClusterState = "issues_detected"
		if ghPRState == "open" {
			report.ClusterState = "pull_requests_open|issues_detected"
		}
	}

	clusterState = report.ClusterState

	_, err = json.MarshalIndent(report, "", "  ")
	if err != nil {
		log.Fatalf("Failed to marshal JSON output: %v", err)
	}

	// fmt.Println(string(output))
}

// Pull Request Checks
func ghPullRequestsCheck() {

	// Ensure required environment variables are set otherwise skip
	if /*token == "" ||*/ ghOwner == "" || ghRepo == "" {
		return
	}

	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/pulls?state=open", ghOwner, ghRepo)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		HandleError("Error creating request:", err)
		//os.Exit(1)
		return
	}

	// Set Authorization header if token is provided (recommended)
	if ghToken != "" {
		req.Header.Set("Authorization", "token "+ghToken)
	}

	req.Header.Set("Accept", "application/vnd.github+json")
	client := &http.Client{Timeout: 10 * time.Second}

	resp, err := client.Do(req)
	if err != nil {
		HandleError("Error sending request:", err)
		//os.Exit(1)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		//fmt.Printf("GitHub API returned status: %s\n", resp.Status)
		HandleError("GitHub API returned status:", errors.New(resp.Status))
		//os.Exit(1)
		return
	}

	var prs []PullRequest
	if err := json.NewDecoder(resp.Body).Decode(&prs); err != nil {
		HandleError("Error decoding response:", err)
		//os.Exit(1)
		return
	}

	if len(prs) == 0 {
		//fmt.Println("No open pull requests found.")
		ghPRState = "none"
		return
	}

	var issues []Issue
	for _, pr := range prs {
		//fmt.Printf("PR #%d: %s by %s\n", pr.Number, pr.Title, pr.User.Login)
		issues = append(issues, Issue{Key: fmt.Sprintf("pr/%d", pr.Number), Type: "PullRequest", Message: pr.Title, Timestamp: time.Now()})
	}
	//ghPRState = "none" // this line to be removed
	if len(issues) > 0 {

		// if current ghPRState is changing from none to open, send a ntfy message
		if ghPRState == "none" {
			ntfyOpts := NtfyOptions{
				Title:    "ntfyTitle Text",
				Priority: 3, // (required)
			}
			err := SendNtfyAlert(fmt.Sprintf("New open pull requests detected: %d", len(issues)), ntfyOpts)
			if err != nil {
				log.Printf("Error sending ntfy alert: %v", err)
			}
		}

		ghPRState = "open"
	}
	pullRequests = issues
}

// Node Checks
func checkNodes(ctx context.Context, clientset *kubernetes.Clientset) []Issue {
	nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Printf("Error fetching nodes: %v", err)
		return nil
	}

	var issues []Issue
	for _, node := range nodes.Items {
		key := fmt.Sprintf("node/%s", node.Name)
		ready := false
		for _, cond := range node.Status.Conditions {
			if cond.Type == v1.NodeReady && cond.Status == v1.ConditionTrue {
				ready = true
				break
			}
		}

		if ready {
			clearIssue(key)
		} else {
			msg := fmt.Sprintf("Node %s is not ready", node.Name)
			reportIssue(key) //, msg)
			issues = append(issues, Issue{Key: key, Type: "Node", Message: msg, Timestamp: time.Now()})
		}
	}
	return issues
}

// Pod Checks
func checkPods(ctx context.Context, clientset *kubernetes.Clientset) []Issue {
	pods, err := clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Printf("Error fetching pods: %v", err)
		return nil
	}

	var issues []Issue
	for _, pod := range pods.Items {
		key := fmt.Sprintf("pod/%s/%s", pod.Namespace, pod.Name)

		switch pod.Status.Phase {
		case v1.PodSucceeded:
			clearIssue(key)
		case v1.PodRunning:
			allReady := true
			for _, cs := range pod.Status.ContainerStatuses {
				if !cs.Ready {
					allReady = false
					break
				}
			}
			if allReady {
				clearIssue(key)
			} else {
				msg := fmt.Sprintf("Pod %s/%s has containers not ready", pod.Namespace, pod.Name)
				reportIssue(key) //, msg)
				issues = append(issues, Issue{Key: key, Type: "Pod", Message: msg, Timestamp: time.Now()})
			}
		default:
			msg := fmt.Sprintf("Pod %s/%s in unexpected phase: %s", pod.Namespace, pod.Name, pod.Status.Phase)
			reportIssue(key) //, msg)
			issues = append(issues, Issue{Key: key, Type: "Pod", Message: msg, Timestamp: time.Now()})
		}
	}
	return issues
}

// Event Checks
func checkEvents(ctx context.Context, clientset *kubernetes.Clientset) []Issue {
	// filter events from the last interval
	since := time.Now().Add(-10 * time.Second)
	events, err := clientset.CoreV1().Events("").List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Printf("Error fetching events: %v", err)
		return nil
	}

	var issues []Issue
	seen := make(map[string]time.Time)

	for _, e := range events.Items {
		if e.Type != v1.EventTypeWarning {
			continue
		}
		if e.LastTimestamp.Time.Before(since) {
			continue
		}

		key := fmt.Sprintf("%s/%s:%s", e.Namespace, e.InvolvedObject.Name, e.Reason)
		if last, ok := seen[key]; ok && time.Since(last) < 5*time.Minute {
			continue
		}
		seen[key] = e.LastTimestamp.Time

		// Skip event if resource is healthy
		if !isResourceUnhealthy(ctx, clientset, e) {
			clearIssue(key)
			continue
		}

		msg := fmt.Sprintf("%s/%s: %s — %s", e.Namespace, e.InvolvedObject.Name, e.Reason, e.Message)
		reportIssue(key) //, msg)
		issues = append(issues, Issue{Key: key, Type: "Event", Message: msg, Timestamp: time.Now()})
	}

	return issues
}

// Resource Health Helper
func isResourceUnhealthy(ctx context.Context, clientset *kubernetes.Clientset, e v1.Event) bool {
	switch e.InvolvedObject.Kind {

	// Check Pods
	case "Pod":
		pod, err := clientset.CoreV1().Pods(e.Namespace).Get(ctx, e.InvolvedObject.Name, metav1.GetOptions{})
		if err != nil {
			return true
		}
		if pod.Status.Phase == v1.PodSucceeded {
			return false
		}
		if pod.Status.Phase == v1.PodRunning {
			for _, cs := range pod.Status.ContainerStatuses {
				if !cs.Ready {
					return true
				}
			}
			return false
		}
		return true

	// Check Nodes
	case "Node":
		node, err := clientset.CoreV1().Nodes().Get(ctx, e.InvolvedObject.Name, metav1.GetOptions{})
		if err != nil {
			return true
		}
		for _, cond := range node.Status.Conditions {
			if cond.Type == v1.NodeReady && cond.Status != v1.ConditionTrue {
				return true
			}
		}
		return false

	// Default: assume unhealthy
	default:
		return true
	}
}

// Issue State Management
// func reportIssue(key, msg string) {
func reportIssue(key string) {
	//if _, exists := knownIssues[key]; !exists {
	//log.Printf("⚠️  %s", msg)
	//}
	knownIssues[key] = time.Now()
}

func clearIssue(key string) {
	//if _, exists := knownIssues[key]; exists {
	//log.Printf("✅ Issue resolved: %s", key)
	delete(knownIssues, key)
	//}
}

var ntfyUrl = ""                // os.Getenv("NTFY_URL")
var ntfyTopic = ""              // os.Getenv("NTFY_TOPIC")
var ntfyPriority = "high"       // os.Getenv("NTFY_PRIORITY") // low, default, high, urgent
var ntfyTags = ""               // os.Getenv("NTFY_TAGS") // comma-separated list of tags
var ntfyTitle = "Cluster Alert" // os.Getenv("NTFY_TITLE") // default: "Cluster Alert"

var ntfyMessage = ""    // os.Getenv("NTFY_MESSAGE") // default: "An issue has been detected in the cluster."
var ntfyEnabled = false // os.Getenv("NTFY_ENABLED") == "true"
var ntfyMaxRetries = 3  // os.Getenv("NTFY_MAX_RETRIES") // default: 3
var ntfyRetryDelay = 5  // os.Getenv("NTFY_RETRY_DELAY") // seconds, default: 5
var ntfyLastSent time.Time
var ntfyRateLimit = 60 // seconds, default: 60
var ntfyErrorCount int
var ntfyErrorLimit = 5  // default: 5
var ntfyErrorDelay = 10 // seconds, default: 10
var ntfyLastError time.Time

type NtfyOptions struct {
	Server   string
	Topic    string
	Title    string
	Priority int    // 1–5 (ntfy standard)
	Icon     string // URL or emoji
	Tags     string // comma-separated tags (optional)
}

func SendNtfyAlert(message string, opts NtfyOptions) error {
	if opts.Server == "" {
		opts.Server = os.Getenv("NTFY_URL") // "https://ntfy.sh"
	}
	if opts.Server == "" {
		// server is still empty, so just return without doing anything
		return nil
	}

	if opts.Topic == "" {
		opts.Server = os.Getenv("NTFY_TOPIC") // "clusterbulb"
	}
	if opts.Topic == "" {
		return fmt.Errorf("ntfy topic cannot be empty")
	}
	if opts.Priority < 1 || opts.Priority > 5 {
		return fmt.Errorf("priority must be between 1 and 5")
	}

	url := fmt.Sprintf("%s/%s", opts.Server, opts.Topic)

	req, err := http.NewRequest("POST", url, bytes.NewBuffer([]byte(message)))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	// Add headers
	if opts.Title != "" {
		req.Header.Set("Title", opts.Title)
	}
	req.Header.Set("Priority", fmt.Sprintf("%d", opts.Priority))

	if opts.Icon != "" {
		req.Header.Set("Icon", opts.Icon)
	}
	if opts.Tags != "" {
		req.Header.Set("Tags", opts.Tags)
	}

	// Send request
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send ntfy request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("ntfy returned unexpected status: %s", resp.Status)
	}

	return nil
}
