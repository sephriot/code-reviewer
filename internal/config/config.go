package config

import (
	"flag"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	GithubToken   string        `yaml:"github_token"`
	GithubUsername string       `yaml:"github_username"`
	ReviewTool    string        `yaml:"review_tool"`
	ClaudeModel   string        `yaml:"claude_model"`
	ReviewEffort  string        `yaml:"review_effort"`
	ReviewTimeout time.Duration `yaml:"review_timeout"`
	ReviewPromptPath string     `yaml:"review_prompt_path"`
	PollInterval  time.Duration `yaml:"poll_interval"`
	DBPath        string        `yaml:"db_path"`
	LogLevel      string        `yaml:"log_level"`
	DryRun        bool          `yaml:"dry_run"`
	ShowThinking  bool          `yaml:"show_thinking"`
	AtlasEnabled  bool          `yaml:"atlas_enabled"`
	WebEnabled    bool          `yaml:"web_enabled"`
	WebHost       string        `yaml:"web_host"`
	WebPort       int           `yaml:"web_port"`

	Repositories    []string `yaml:"repositories"`
	PRAuthors       []string `yaml:"pr_authors"`

	SoundEnabled               bool   `yaml:"sound_enabled"`
	StartupSoundsEnabled       bool   `yaml:"startup_sounds_enabled"`
	SpeechRate                 int    `yaml:"speech_rate"`
	SoundFile                  string `yaml:"sound_file"`
	ApprovalSoundEnabled       bool   `yaml:"approval_sound_enabled"`
	ApprovalSoundFile          string `yaml:"approval_sound_file"`
	TimeoutSoundEnabled        bool   `yaml:"timeout_sound_enabled"`
	TimeoutSoundFile           string `yaml:"timeout_sound_file"`
	HumanReviewSoundEnabled    bool   `yaml:"human_review_sound_enabled"`
	HumanReviewSoundFile       string `yaml:"human_review_sound_file"`
	ReviewStartedSoundEnabled  bool   `yaml:"review_started_sound_enabled"`
	ReviewStartedSoundFile     string `yaml:"review_started_sound_file"`
	MergedOrClosedSoundEnabled bool   `yaml:"merged_or_closed_sound_enabled"`
	MergedOrClosedSoundFile    string `yaml:"merged_or_closed_sound_file"`
	OwnPRReadySoundEnabled     bool   `yaml:"own_pr_ready_sound_enabled"`
	OwnPRReadySoundFile        string `yaml:"own_pr_ready_sound_file"`
	OwnPRNeedsAttentionSoundEnabled bool `yaml:"own_pr_needs_attention_sound_enabled"`
	OwnPRNeedsAttentionSoundFile    string `yaml:"own_pr_needs_attention_sound_file"`

	OwnPRMode string `yaml:"own_pr_mode"`
}

func (c *Config) RepoFilterRegex() []*regexp.Regexp {
	return compilePatterns(c.Repositories)
}

func (c *Config) AuthorFilterRegex() []*regexp.Regexp {
	return compilePatterns(c.PRAuthors)
}

func compilePatterns(patterns []string) []*regexp.Regexp {
	var res []*regexp.Regexp
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		re, err := regexp.Compile(p)
		if err != nil {
			continue
		}
		res = append(res, re)
	}
	return res
}

func Load() (*Config, error) {
	cfg := defaultConfig()
	cfg.loadYAML()
	cfg.loadEnv()
	cfg.loadFlags()
	return cfg, cfg.validate()
}

func defaultConfig() *Config {
	return &Config{
		ReviewTool:    "CLAUDE",
		ReviewTimeout: 15 * time.Minute,
		PollInterval:  1 * time.Minute,
		DBPath:        "data/reviews.db",
		LogLevel:      "INFO",
		WebEnabled:    true,
		WebHost:       "127.0.0.1",
		WebPort:       8000,
		SpeechRate:    200,
		SoundEnabled:  true,
		StartupSoundsEnabled: true,

		ApprovalSoundEnabled:           true,
		TimeoutSoundEnabled:            true,
		HumanReviewSoundEnabled:        true,
		ReviewStartedSoundEnabled:      true,
		MergedOrClosedSoundEnabled:     true,
		OwnPRReadySoundEnabled:         true,
		OwnPRNeedsAttentionSoundEnabled: true,
		OwnPRMode:                      "off",
	}
}

func (c *Config) loadYAML() {
	path := os.Getenv("CONFIG_PATH")
	if path == "" {
		path = "config.yaml"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	_ = yaml.Unmarshal(data, c)
}

func (c *Config) loadEnv() {
	if v, ok := os.LookupEnv("GITHUB_TOKEN"); ok {
		c.GithubToken = v
	}
	if v, ok := os.LookupEnv("GITHUB_USERNAME"); ok {
		c.GithubUsername = v
	}
	if v, ok := os.LookupEnv("REVIEW_TOOL"); ok {
		c.ReviewTool = v
	}
	if v, ok := os.LookupEnv("CLAUDE_MODEL"); ok {
		c.ClaudeModel = v
	}
	if v, ok := os.LookupEnv("REVIEW_EFFORT"); ok {
		c.ReviewEffort = v
	}
	if v, ok := os.LookupEnv("REVIEW_TIMEOUT"); ok {
		if sec, err := strconv.Atoi(v); err == nil {
			c.ReviewTimeout = time.Duration(sec) * time.Second
		}
	}
	if v, ok := os.LookupEnv("PROMPT_FILE"); ok {
		c.ReviewPromptPath = v
	}
	if v, ok := os.LookupEnv("POLL_INTERVAL"); ok {
		if sec, err := strconv.Atoi(v); err == nil {
			c.PollInterval = time.Duration(sec) * time.Second
		}
	}
	if v, ok := os.LookupEnv("DATABASE_PATH"); ok {
		c.DBPath = v
	}
	if v, ok := os.LookupEnv("LOG_LEVEL"); ok {
		c.LogLevel = v
	}
	if v, ok := os.LookupEnv("DRY_RUN"); ok {
		c.DryRun = v == "true"
	}
	if v, ok := os.LookupEnv("SHOW_THINKING"); ok {
		c.ShowThinking = v == "true"
	}
	if v, ok := os.LookupEnv("ATLAS_ENABLED"); ok {
		c.AtlasEnabled = v == "true"
	}
	if v, ok := os.LookupEnv("WEB_ENABLED"); ok {
		c.WebEnabled = v == "true"
	}
	if v, ok := os.LookupEnv("WEB_HOST"); ok {
		c.WebHost = v
	}
	if v, ok := os.LookupEnv("WEB_PORT"); ok {
		if port, err := strconv.Atoi(v); err == nil {
			c.WebPort = port
		}
	}
	if v, ok := os.LookupEnv("REPOSITORIES"); ok {
		c.Repositories = splitAndTrim(v)
	}
	if v, ok := os.LookupEnv("PR_AUTHORS"); ok {
		c.PRAuthors = splitAndTrim(v)
	}
	if v, ok := os.LookupEnv("SOUND_ENABLED"); ok {
		c.SoundEnabled = v == "true"
	}
	if v, ok := os.LookupEnv("STARTUP_SOUNDS_ENABLED"); ok {
		c.StartupSoundsEnabled = v == "true"
	}
	if v, ok := os.LookupEnv("SPEECH_RATE"); ok {
		if rate, err := strconv.Atoi(v); err == nil {
			c.SpeechRate = rate
		}
	}
	if v, ok := os.LookupEnv("SOUND_FILE"); ok {
		c.SoundFile = v
	}
	if v, ok := os.LookupEnv("APPROVAL_SOUND_ENABLED"); ok {
		c.ApprovalSoundEnabled = v == "true"
	}
	if v, ok := os.LookupEnv("APPROVAL_SOUND_FILE"); ok {
		c.ApprovalSoundFile = v
	}
	if v, ok := os.LookupEnv("TIMEOUT_SOUND_ENABLED"); ok {
		c.TimeoutSoundEnabled = v == "true"
	}
	if v, ok := os.LookupEnv("TIMEOUT_SOUND_FILE"); ok {
		c.TimeoutSoundFile = v
	}
	if v, ok := os.LookupEnv("HUMAN_REVIEW_SOUND_ENABLED"); ok {
		c.HumanReviewSoundEnabled = v == "true"
	}
	if v, ok := os.LookupEnv("HUMAN_REVIEW_SOUND_FILE"); ok {
		c.HumanReviewSoundFile = v
	}
	if v, ok := os.LookupEnv("REVIEW_STARTED_SOUND_ENABLED"); ok {
		c.ReviewStartedSoundEnabled = v == "true"
	}
	if v, ok := os.LookupEnv("REVIEW_STARTED_SOUND_FILE"); ok {
		c.ReviewStartedSoundFile = v
	}
	if v, ok := os.LookupEnv("MERGED_OR_CLOSED_SOUND_ENABLED"); ok {
		c.MergedOrClosedSoundEnabled = v == "true"
	}
	if v, ok := os.LookupEnv("MERGED_OR_CLOSED_SOUND_FILE"); ok {
		c.MergedOrClosedSoundFile = v
	}
	if v, ok := os.LookupEnv("OWN_PR_READY_SOUND_ENABLED"); ok {
		c.OwnPRReadySoundEnabled = v == "true"
	}
	if v, ok := os.LookupEnv("OWN_PR_READY_SOUND_FILE"); ok {
		c.OwnPRReadySoundFile = v
	}
	if v, ok := os.LookupEnv("OWN_PR_NEEDS_ATTENTION_SOUND_ENABLED"); ok {
		c.OwnPRNeedsAttentionSoundEnabled = v == "true"
	}
	if v, ok := os.LookupEnv("OWN_PR_NEEDS_ATTENTION_SOUND_FILE"); ok {
		c.OwnPRNeedsAttentionSoundFile = v
	}
	if v, ok := os.LookupEnv("OWN_PR_MODE"); ok {
		c.OwnPRMode = v
	}
}

func (c *Config) loadFlags() {
	configPath := flag.String("config", "", "path to YAML config file")
	port := flag.Int("port", 0, "web server port")
	host := flag.String("host", "", "web server host")
	logLevel := flag.String("log", "", "log level (DEBUG, INFO, WARN, ERROR)")
	flag.Parse()

	if *configPath != "" {
		data, err := os.ReadFile(*configPath)
		if err == nil {
			_ = yaml.Unmarshal(data, c)
		}
	}
	if *port != 0 {
		c.WebPort = *port
	}
	if *host != "" {
		c.WebHost = *host
	}
	if *logLevel != "" {
		c.LogLevel = *logLevel
	}
}

func (c *Config) validate() error {
	if c.GithubToken == "" {
		return fmt.Errorf("GITHUB_TOKEN is required")
	}
	if c.GithubUsername == "" {
		return fmt.Errorf("GITHUB_USERNAME is required")
	}
	if c.PollInterval <= 0 {
		return fmt.Errorf("POLL_INTERVAL must be positive")
	}
	if c.ReviewTimeout <= 0 {
		return fmt.Errorf("REVIEW_TIMEOUT must be positive")
	}
	if c.WebPort < 1 || c.WebPort > 65535 {
		return fmt.Errorf("WEB_PORT must be between 1 and 65535")
	}
	return nil
}

func splitAndTrim(s string) []string {
	parts := strings.Split(s, ",")
	var res []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			res = append(res, p)
		}
	}
	return res
}
