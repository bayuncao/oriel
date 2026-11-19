package provider

import (
	"slices"
	"strings"
)

// Feed 是一家狀態頁的格式。
type Feed string

const (
	// FeedStatuspage：Atlassian Statuspage 或相容它的平台（incident.io 等）的
	// /api/v2/summary.json 與 /api/v2/incidents.json；Source 是狀態頁的根網址。
	FeedStatuspage Feed = "statuspage"
	// FeedFlashduty：Flashduty 狀態頁的 RSS；Source 是 feed 的網址。
	FeedFlashduty Feed = "flashduty"
	// FeedGoogleCloud：Google Cloud 的 incidents.json；Source 是它的網址。
	FeedGoogleCloud Feed = "google_cloud"
	// FeedAWSHealth：AWS Health 的當前事件（UTF-16 的 JSON）；Source 是它的網址。
	FeedAWSHealth Feed = "aws_health"
)

// VendorGroup 是清單裡的一組，決定卡片上的先後（PS01）。
type VendorGroup string

const (
	GroupModels    VendorGroup = "models"    // 模型廠商
	GroupPlatforms VendorGroup = "platforms" // 推理平台
	GroupTools     VendorGroup = "tools"     // 其他
)

// Vendor 是一家有可讀狀態頁的廠商。
type Vendor struct {
	ID    string
	Name  string
	Group VendorGroup
	// Page 是給人看的狀態頁。
	Page   string
	Feed   Feed
	Source string
	// Track 決定哪些組件（Google 是受影響的產品，AWS 是服務）算進這一行；
	// nil 表示整個狀態頁（當前狀態取整頁的指標，歷史算整頁的事故）。
	Track func(name string) bool
	// Label 是狀態頁沒給組件清單時（Google、AWS、Flashduty 還沒提到時），
	// 卡片上名稱下面那一行寫的組件。
	Label []string
}

// named：名稱是其中之一的組件（或組件組）。
func named(names ...string) func(string) bool {
	return func(n string) bool { return slices.Contains(names, n) }
}

func containing(s string) func(string) bool {
	return func(n string) bool { return strings.Contains(n, s) }
}

func prefixed(p string) func(string) bool {
	return func(n string) bool { return strings.HasPrefix(n, p) }
}

func statuspage(id, name string, group VendorGroup, root string, track func(string) bool) Vendor {
	return Vendor{ID: id, Name: name, Group: group, Page: root, Feed: FeedStatuspage, Source: root, Track: track}
}

// Catalog 是內建的廠商清單，依卡片上的順序（PS01，卡片預設只顯示前六家，PS31；調研見
// docs/research/provider-status.md）。追蹤的組件是 API 與編碼 agent 用到的；
// 組件只是一個個模型、名稱常換的（Fireworks、Cerebras、Jina AI），以及沒有組件的
// （ElevenLabs）算整個狀態頁。
func Catalog() []Vendor {
	return []Vendor{
		statuspage("openai", "OpenAI", GroupModels, "https://status.openai.com", named("Responses", "Chat Completions")),
		statuspage("anthropic", "Anthropic", GroupModels, "https://status.claude.com", named("Claude API (api.anthropic.com)", "Claude Code")),
		{ID: "google", Name: "Google", Group: GroupModels, Page: "https://status.cloud.google.com", Feed: FeedGoogleCloud,
			Source: "https://status.cloud.google.com/incidents.json", Track: named("Vertex Gemini API"), Label: []string{"Vertex Gemini API"}},
		{ID: "deepseek", Name: "DeepSeek", Group: GroupModels, Page: "https://status.deepseek.com", Feed: FeedFlashduty,
			Source: "https://status.deepseek.com/feed.rss", Track: containing("API Service"), Label: []string{"API Service"}},
		statuspage("moonshot", "Moonshot AI", GroupModels, "https://status.moonshot.cn", named("API Service", "Open API")),
		statuspage("minimax", "MiniMax", GroupModels, "https://status.minimax.io", named("Large Language Models (LLM)")),
		statuspage("mistral", "Mistral AI", GroupModels, "https://status.mistral.ai", named("Completion API", "Agents API")),
		statuspage("cohere", "Cohere", GroupModels, "https://status.cohere.com", named("All Endpoints")),
		statuspage("ai21", "AI21", GroupModels, "https://status.ai21.com", named("Jamba Models")),
		statuspage("perplexity", "Perplexity", GroupModels, "https://status.perplexity.com", named("API")),
		statuspage("groq", "Groq", GroupPlatforms, "https://groqstatus.com", named("API")),
		statuspage("fireworks", "Fireworks", GroupPlatforms, "https://status.fireworks.ai", nil),
		statuspage("cerebras", "Cerebras", GroupPlatforms, "https://status.cerebras.ai", nil),
		statuspage("baseten", "Baseten", GroupPlatforms, "https://status.baseten.co", named("Model APIs", "Dedicated Inference")),
		statuspage("anyscale", "Anyscale", GroupPlatforms, "https://status.anyscale.com", named("SDK/API")),
		{ID: "bedrock", Name: "Amazon Bedrock", Group: GroupPlatforms, Page: "https://health.aws.amazon.com/health/status", Feed: FeedAWSHealth,
			Source: "https://health.aws.amazon.com/public/currentevents", Track: prefixed("bedrock-"), Label: []string{"Amazon Bedrock"}},
		statuspage("github", "GitHub", GroupTools, "https://www.githubstatus.com", named("Copilot", "Copilot AI Model Providers")),
		statuspage("windsurf", "Windsurf", GroupTools, "https://status.windsurf.com", named("Cascade", "Windsurf Tab")),
		statuspage("jina", "Jina AI", GroupTools, "https://status.jina.ai", nil),
		statuspage("elevenlabs", "ElevenLabs", GroupTools, "https://status.elevenlabs.io", nil),
	}
}
