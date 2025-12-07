//go:build !windows
// +build !windows

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const (
	// Bot Token (必需) - 从 @BotFather 获取
	BotToken = "11111:A2222qHJu2kAuwVelu8gKjgDOH24I1M"

	// 订阅 API 配置
	SubscriptionAPIHost = "111.111.111.111:12345"
	SubscriptionAPIKey  = "123456"
)

// 示例: var AllowedUsers = map[int64]bool{123456789: true, 987654321: true}
var AllowedUsers map[int64]bool = nil

// TDL 脚本路径 (动态获取)
var TDLScriptPath string

// ==================== 配置区域结束 ====================

// Task 表示一个正在运行的任务
type Task struct {
	ID      int
	UserID  int64
	Cmd     *exec.Cmd
	Cancel  context.CancelFunc
	Message *tgbotapi.Message
	PGID    int
}

// QueuedTask 表示队列中的任务
type QueuedTask struct {
	Link        string
	Message     *tgbotapi.Message
	UserID      int64
	StatusMsg   *tgbotapi.Message // 状态消息
	TaskID      int               // 任务ID
	Cancelled   bool              // 是否已取消
	CancelMutex sync.Mutex        // 取消操作的互斥锁
	Index       int               // 如果是汇总消息, 该任务在汇总消息中的行索引
	Shared      bool              // 是否共享汇总消息
}

// TaskManager 管理所有活跃的任务和队列
type TaskManager struct {
	mu              sync.RWMutex
	tasks           map[int64]map[int]*Task       // user_id -> task_id -> task
	counters        map[int64]int                 // user_id -> counter
	queue           chan *QueuedTask              // 任务队列
	queuedTasks     map[int64]map[int]*QueuedTask // user_id -> task_id -> queued task (用于取消队列中的任务)
	currentTask     *Task                         // 当前正在执行的任务
	queueProcessing bool                          // 队列是否正在处理
	// 汇总消息缓存: chatID -> messageID -> []lines
	summaryLines map[int64]map[int][]string
	// 汇总消息的键盘缓存: chatID -> messageID -> markup
	summaryKeyboards map[int64]map[int]*tgbotapi.InlineKeyboardMarkup
	// 汇总消息待完成计数: chatID -> messageID -> remaining count
	summaryPendingCounts map[int64]map[int]int
}

func NewTaskManager() *TaskManager {
	return &TaskManager{
		tasks:                make(map[int64]map[int]*Task),
		counters:             make(map[int64]int),
		queue:                make(chan *QueuedTask, 100), // 缓冲队列，最多100个任务
		queuedTasks:          make(map[int64]map[int]*QueuedTask),
		currentTask:          nil,
		queueProcessing:      false,
		summaryLines:         make(map[int64]map[int][]string),
		summaryKeyboards:     make(map[int64]map[int]*tgbotapi.InlineKeyboardMarkup),
		summaryPendingCounts: make(map[int64]map[int]int),
	}
}

// AddTask 添加任务
func (tm *TaskManager) AddTask(userID int64, task *Task) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	if tm.tasks[userID] == nil {
		tm.tasks[userID] = make(map[int]*Task)
	}
	tm.counters[userID]++
	task.ID = tm.counters[userID]
	tm.tasks[userID][task.ID] = task
}

// RemoveTask 移除任务
func (tm *TaskManager) RemoveTask(userID int64, taskID int) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	if tasks, exists := tm.tasks[userID]; exists {
		delete(tasks, taskID)
		if len(tasks) == 0 {
			delete(tm.tasks, userID)
		}
	}
}

// GetTask 获取任务
func (tm *TaskManager) GetTask(userID int64, taskID int) (*Task, bool) {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	if tasks, exists := tm.tasks[userID]; exists {
		task, ok := tasks[taskID]
		return task, ok
	}
	return nil, false
}

// CountUserTasks 统计用户任务数
func (tm *TaskManager) CountUserTasks(userID int64) int {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	return len(tm.tasks[userID])
}

// GetQueueSize 获取队列大小
func (tm *TaskManager) GetQueueSize() int {
	return len(tm.queue)
}

// GetCurrentTask 获取当前正在执行的任务
func (tm *TaskManager) GetCurrentTask() *Task {
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	return tm.currentTask
}

// SetCurrentTask 设置当前正在执行的任务
func (tm *TaskManager) SetCurrentTask(task *Task) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tm.currentTask = task
}

// EnqueueTask 将任务加入队列
func (tm *TaskManager) EnqueueTask(task *QueuedTask) {
	tm.mu.Lock()
	if tm.queuedTasks[task.UserID] == nil {
		tm.queuedTasks[task.UserID] = make(map[int]*QueuedTask)
	}
	tm.queuedTasks[task.UserID][task.TaskID] = task
	tm.mu.Unlock()

	tm.queue <- task
}

// RemoveQueuedTask 从队列任务映射中移除
func (tm *TaskManager) RemoveQueuedTask(userID int64, taskID int) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	if tasks, exists := tm.queuedTasks[userID]; exists {
		delete(tasks, taskID)
		if len(tasks) == 0 {
			delete(tm.queuedTasks, userID)
		}
	}
}

// InitSummary 初始化汇总消息的行缓存
func (tm *TaskManager) InitSummary(chatID int64, messageID int, lines []string, keyboard *tgbotapi.InlineKeyboardMarkup) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if tm.summaryLines[chatID] == nil {
		tm.summaryLines[chatID] = make(map[int][]string)
	}
	tm.summaryLines[chatID][messageID] = lines
	if keyboard != nil {
		if tm.summaryKeyboards[chatID] == nil {
			tm.summaryKeyboards[chatID] = make(map[int]*tgbotapi.InlineKeyboardMarkup)
		}
		tm.summaryKeyboards[chatID][messageID] = keyboard
	}
	// 初始化待完成计数
	if tm.summaryPendingCounts[chatID] == nil {
		tm.summaryPendingCounts[chatID] = make(map[int]int)
	}
	tm.summaryPendingCounts[chatID][messageID] = len(lines)
}

// DecrementSummaryPending 将汇总消息的待完成计数减一并返回剩余数量
func (tm *TaskManager) DecrementSummaryPending(chatID int64, messageID int) int {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if tm.summaryPendingCounts[chatID] == nil {
		return 0
	}
	if _, ok := tm.summaryPendingCounts[chatID][messageID]; !ok {
		return 0
	}
	tm.summaryPendingCounts[chatID][messageID]--
	remaining := tm.summaryPendingCounts[chatID][messageID]
	if remaining <= 0 {
		delete(tm.summaryPendingCounts[chatID], messageID)
		if len(tm.summaryPendingCounts[chatID]) == 0 {
			delete(tm.summaryPendingCounts, chatID)
		}
		// 同时删除键盘缓存，以便后续编辑不会再恢复按钮
		if km, ok := tm.summaryKeyboards[chatID]; ok {
			delete(km, messageID)
			if len(km) == 0 {
				delete(tm.summaryKeyboards, chatID)
			}
		}
		return 0
	}
	return remaining
}

// GetSummaryLines 返回缓存的汇总行（只读）
func (tm *TaskManager) GetSummaryLines(chatID int64, messageID int) ([]string, bool) {
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	if tm.summaryLines[chatID] == nil {
		return nil, false
	}
	lines, ok := tm.summaryLines[chatID][messageID]
	return lines, ok
}

// UpdateSummaryLine 更新缓存中的一行并返回最新的所有行和对应的键盘（如果有）
func (tm *TaskManager) UpdateSummaryLine(chatID int64, messageID int, index int, text string) ([]string, *tgbotapi.InlineKeyboardMarkup) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if tm.summaryLines[chatID] == nil {
		return nil, nil
	}
	lines, ok := tm.summaryLines[chatID][messageID]
	if !ok {
		return nil, nil
	}
	if index >= 0 && index < len(lines) {
		lines[index] = text
		tm.summaryLines[chatID][messageID] = lines
	}
	var kb *tgbotapi.InlineKeyboardMarkup
	if km, okk := tm.summaryKeyboards[chatID]; okk {
		if m, okm := km[messageID]; okm {
			kb = m
		}
	}
	return lines, kb
}

// GetQueuedTask 获取队列中的任务
func (tm *TaskManager) GetQueuedTask(userID int64, taskID int) (*QueuedTask, bool) {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	if tasks, exists := tm.queuedTasks[userID]; exists {
		task, ok := tasks[taskID]
		return task, ok
	}
	return nil, false
}

// CancelQueuedTask 取消队列中的任务
func (tm *TaskManager) CancelQueuedTask(userID int64, taskID int) bool {
	// We will remove the queued entry under lock, but if the task is already
	// running we must also cancel the running Task. To avoid deadlocks we
	// capture the fact that we found and removed the queued entry while holding
	// the lock, then release the lock and call CancelTask which obtains its
	// own locks.
	tm.mu.Lock()
	var found bool
	if tasks, exists := tm.queuedTasks[userID]; exists {
		if task, ok := tasks[taskID]; ok {
			task.CancelMutex.Lock()
			task.Cancelled = true
			task.CancelMutex.Unlock()

			delete(tasks, taskID)
			if len(tasks) == 0 {
				delete(tm.queuedTasks, userID)
			}
			found = true
		}
	}
	tm.mu.Unlock()

	if found {
		// If the corresponding Task is already running, ensure it is also
		// cancelled so the user doesn't need to click again.
		_ = tm.CancelTask(userID, taskID)
		return true
	}
	return false
}

// CancelTask 取消任务
func (tm *TaskManager) CancelTask(userID int64, taskID int) bool {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	if tasks, exists := tm.tasks[userID]; exists {
		if task, ok := tasks[taskID]; ok {
			if task.Cancel != nil {
				task.Cancel()
			}
			// 尝试先终止整个进程组（类 Unix 系统），以确保子进程也被清理
			if task.Cmd != nil && task.Cmd.Process != nil {
				p := task.Cmd.Process
				// 如果我们事先记录了 PGID，优先使用它来终止整组进程。
				if task.PGID != 0 {
					// 先尝试温和终止，再强制结束
					_ = syscall.Kill(-task.PGID, syscall.SIGTERM)
					time.Sleep(500 * time.Millisecond)
					_ = syscall.Kill(-task.PGID, syscall.SIGKILL)
				} else {
					// 先尝试通过进程组清理（在 Unix 上实现），否则回退到直接 Kill
					if err := killProcessGroup(p.Pid); err != nil {
						_ = p.Kill()
					}
				}
			}
			delete(tasks, taskID)
			if len(tasks) == 0 {
				delete(tm.tasks, userID)
			}
			return true
		}
	}
	return false
}

// SubscriptionRequest 订阅请求结构
type SubscriptionRequest struct {
	SubURL string `json:"sub_url"`
}

// SubscriptionResponse 订阅响应结构
type SubscriptionResponse struct {
	Message string `json:"message,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Bot 主结构
type Bot struct {
	api         *tgbotapi.BotAPI
	taskManager *TaskManager
	logger      *log.Logger
}

// NewBot 创建新的 Bot 实例
func NewBot(token string) (*Bot, error) {
	api, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return nil, fmt.Errorf("创建 Bot API 失败: %w", err)
	}

	api.Debug = false

	return &Bot{
		api:         api,
		taskManager: NewTaskManager(),
		logger:      log.New(os.Stdout, "[BOT] ", log.LstdFlags|log.Lshortfile),
	}, nil
}

// checkUserPermission 检查用户权限
func checkUserPermission(userID int64) bool {
	if AllowedUsers == nil {
		return true // 未配置白名单，允许所有用户
	}
	return AllowedUsers[userID]
}

// handleStart 处理 /start 命令
func (b *Bot) handleStart(message *tgbotapi.Message) {
	user := message.From
	b.logger.Printf("用户 %d (%s) 发送了 /start 命令", user.ID, user.UserName)

	welcomeText := fmt.Sprintf(
		"👋 你好 %s!\n\n"+
			"🤖 这是一个多功能机器人\n\n"+
			"📋 支持功能:\n"+
			"• TDL 转发 - 发送 Telegram 链接 (https://t.me/xxx)\n"+
			"• 订阅管理 - 发送订阅链接 (http/https 格式)\n\n"+
			"💡 直接发送链接即可，Bot 会自动识别类型",
		user.FirstName,
	)

	msg := tgbotapi.NewMessage(message.Chat.ID, welcomeText)
	msg.ReplyToMessageID = message.MessageID
	b.api.Send(msg)
}

// handleHelp 处理 /help 命令
func (b *Bot) handleHelp(message *tgbotapi.Message) {
	helpText := "📖 使用帮助\n\n" +
		"1️⃣ 发送 Telegram 链接进行转发\n" +
		"   格式: https://t.me/channel/123\n\n" +
		"2️⃣ 发送订阅链接进行添加\n" +
		"   格式: 任意 http/https 链接 (非 t.me)\n\n" +
		"3️⃣ 支持的命令:\n" +
		"   /start - 开始使用\n" +
		"   /help - 查看帮助\n" +
		"   /status - 检查状态\n\n" +
		"❓ 遇到问题请联系管理员"

	msg := tgbotapi.NewMessage(message.Chat.ID, helpText)
	msg.ReplyToMessageID = message.MessageID
	b.api.Send(msg)
}

// handleStatus 处理 /status 命令
func (b *Bot) handleStatus(message *tgbotapi.Message) {
	userID := message.From.ID

	// 检查 TDL 脚本是否存在
	scriptExists := "❌ 未找到"
	if _, err := os.Stat(TDLScriptPath); err == nil {
		scriptExists = "✅ 存在"
	}

	// 获取队列状态
	queueSize := b.taskManager.GetQueueSize()
	currentTask := b.taskManager.GetCurrentTask()
	isProcessing := "空闲"
	var processingInfo string

	if currentTask != nil {
		isProcessing = "处理中"
		processingInfo = fmt.Sprintf("\n⚡ 正在处理: 任务 #%d (用户 %d)", currentTask.ID, currentTask.UserID)
	}

	statusText := fmt.Sprintf(
		"✅ Bot 运行正常\n"+
			"📁 TDL 脚本: %s (%s)\n"+
			"🌐 订阅 API: %s\n"+
			"👤 当前用户: %d\n"+
			"📊 队列模式: 排队执行 (一次一个)\n"+
			"🔄 当前状态: %s\n"+
			"📋 等待队列: %d 个任务%s",
		TDLScriptPath, scriptExists,
		SubscriptionAPIHost,
		userID,
		isProcessing,
		queueSize,
		processingInfo,
	)

	msg := tgbotapi.NewMessage(message.Chat.ID, statusText)
	msg.ReplyToMessageID = message.MessageID
	b.api.Send(msg)
}

// addSubscription 添加订阅到 API
func (b *Bot) addSubscription(subURL string) (bool, string) {
	apiURL := fmt.Sprintf("http://%s/api/config/add", SubscriptionAPIHost)

	reqBody := SubscriptionRequest{SubURL: subURL}
	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		b.logger.Printf("JSON 序列化失败: %v", err)
		return false, fmt.Sprintf("❌ 请求失败: %v", err)
	}

	req, err := http.NewRequest("POST", apiURL, bytes.NewBuffer(jsonData))
	if err != nil {
		b.logger.Printf("创建请求失败: %v", err)
		return false, fmt.Sprintf("❌ 请求失败: %v", err)
	}

	req.Header.Set("X-API-Key", SubscriptionAPIKey)
	req.Header.Set("Content-Type", "application/json")

	b.logger.Printf("发送订阅请求到 %s", apiURL)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		b.logger.Printf("订阅 API 请求失败: %v", err)
		if os.IsTimeout(err) {
			return false, "❌ 请求超时，请稍后重试"
		}
		return false, "❌ 无法连接到服务器"
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		b.logger.Printf("读取响应失败: %v", err)
		return false, "❌ 读取响应失败"
	}

	var response SubscriptionResponse
	if err := json.Unmarshal(body, &response); err != nil {
		b.logger.Printf("解析响应失败: %v", err)
		return false, fmt.Sprintf("❌ 订阅添加失败 (状态码: %d)", resp.StatusCode)
	}

	if resp.StatusCode == 200 {
		successMsg := response.Message
		if successMsg == "" {
			successMsg = "订阅添加成功"
		}
		b.logger.Printf("订阅添加成功: %s - %s", subURL, successMsg)
		return true, fmt.Sprintf("✅ %s", successMsg)
	}

	errorMsg := response.Error
	if errorMsg == "" {
		errorMsg = response.Message
	}
	if errorMsg == "" {
		errorMsg = fmt.Sprintf("订阅添加失败 (状态码: %d)", resp.StatusCode)
	}

	b.logger.Printf("订阅添加失败: %s", errorMsg)

	// 特殊处理重复订阅
	if strings.Contains(errorMsg, "已存在") || strings.Contains(strings.ToLower(errorMsg), "already exists") {
		return false, fmt.Sprintf("⚠️ %s", errorMsg)
	}
	return false, fmt.Sprintf("❌ %s", errorMsg)
}

// handleMessage 处理用户消息
func (b *Bot) handleMessage(message *tgbotapi.Message) {
	user := message.From

	// 权限检查
	if !checkUserPermission(user.ID) {
		b.logger.Printf("未授权用户 %d (%s) 尝试使用 Bot", user.ID, user.UserName)
		msg := tgbotapi.NewMessage(message.Chat.ID, "❌ 您没有权限使用此 Bot")
		msg.ReplyToMessageID = message.MessageID
		b.api.Send(msg)
		return
	}

	text := message.Text
	b.logger.Printf("收到来自用户 %d 的消息: %s", user.ID, truncateString(text, 100))

	// 优先检查是否包含一个或多个 Telegram 链接 (支持多行或空格分隔)
	// 正则支持带或不带协议的 t.me 链接
	reTMe := regexp.MustCompile(`(?i)(?:https?://)?t\.me/[^\s]+`)
	matches := reTMe.FindAllString(text, -1)
	if len(matches) > 0 {
		b.logger.Printf("检测到 %d 个 Telegram 链接", len(matches))

		// 如果包含多条链接, 使用单条汇总消息展示并在内部按行更新
		if len(matches) > 1 {
			// 去重链接，保持原有顺序
			var links []string
			seen := make(map[string]bool)
			for _, raw := range matches {
				link := strings.TrimSpace(raw)
				if !strings.HasPrefix(strings.ToLower(link), "http") {
					link = "https://" + link
				}
				// 规范化用于去重（小写，去尾斜杠）
				norm := strings.ToLower(strings.TrimSuffix(link, "/"))
				if seen[norm] {
					continue
				}
				seen[norm] = true
				links = append(links, link)
			}

			// 为每个链接生成独立 taskID
			taskIDs := make([]int, len(links))
			b.taskManager.mu.Lock()
			for i := range links {
				b.taskManager.counters[user.ID]++
				taskIDs[i] = b.taskManager.counters[user.ID]
			}
			b.taskManager.mu.Unlock()

			// 构造初始原始行（包含链接与排队信息），并为每个链接预创建 QueuedTask（尚未设置 StatusMsg）
			baseQueue := b.taskManager.GetQueueSize()
			rawLines := make([]string, len(links))
			queuedTasks := make([]*QueuedTask, len(links))
			for i, link := range links {
				queuePos := baseQueue + i + 1
				line := fmt.Sprintf("⏳ 任务 #%d - 已加入队列\n%s", taskIDs[i], link)
				if queuePos > 1 {
					line += fmt.Sprintf("\n📋 当前排队位置: 第 %d 位", queuePos)
				} else {
					line += "\n⚡ 即将开始处理"
				}
				rawLines[i] = line

				queuedTasks[i] = &QueuedTask{
					Link:      link,
					Message:   message,
					UserID:    user.ID,
					StatusMsg: nil, // will set after sending
					TaskID:    taskIDs[i],
					Cancelled: false,
					Index:     i,
					Shared:    true,
				}
			}

			// 生成已格式化的展示行（单行样式）用于首次发送和缓存
			formatted := make([]string, len(links))
			for i := range links {
				formatted[i] = b.formatSummaryLine(queuedTasks[i], rawLines[i])
			}

			// 单个按钮用于终止整个汇总消息下的所有任务
			cancelCallback := fmt.Sprintf("cancel_summary_%d", user.ID)
			btn := tgbotapi.NewInlineKeyboardButtonData("🛑 终止全部任务", cancelCallback)
			markup := tgbotapi.NewInlineKeyboardMarkup(tgbotapi.NewInlineKeyboardRow(btn))
			summaryText := strings.Join(formatted, "\n\n")
			msg := tgbotapi.NewMessage(message.Chat.ID, summaryText)
			msg.ReplyToMessageID = message.MessageID
			msg.ReplyMarkup = markup
			sentMsg, err := b.api.Send(msg)
			if err != nil {
				b.logger.Printf("发送汇总消息失败: %v", err)
				return
			}

			// 保存汇总行缓存为已格式化的单行样式
			b.taskManager.InitSummary(message.Chat.ID, sentMsg.MessageID, formatted, &markup)

			// 将每个任务加入队列，设置 StatusMsg 指向同一条状态消息
			for i := range queuedTasks {
				queuedTasks[i].StatusMsg = &sentMsg
				b.taskManager.EnqueueTask(queuedTasks[i])
			}

			return
		}

		// 仅一条链接，按单条任务处理
		raw := matches[0]
		link := raw
		if !strings.HasPrefix(strings.ToLower(link), "http") {
			link = "https://" + link
		}

		// 为该链接生成 taskID
		b.taskManager.mu.Lock()
		b.taskManager.counters[user.ID]++
		taskID := b.taskManager.counters[user.ID]
		b.taskManager.mu.Unlock()

		// 获取队列位置
		queuePosition := b.taskManager.GetQueueSize() + 1

		// 创建终止按钮（单条任务）
		keyboard := tgbotapi.NewInlineKeyboardMarkup(
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("🛑 终止任务", fmt.Sprintf("cancel_%d_%d", user.ID, taskID)),
			),
		)

		// 创建 queuedTask 以便 formatLine 能访问链接与 taskID
		queuedTask := &QueuedTask{
			Link:      link,
			Message:   message,
			UserID:    user.ID,
			StatusMsg: nil,
			TaskID:    taskID,
			Cancelled: false,
			Shared:    false,
		}

		// 构造单行初始状态（与汇总样式一致）
		var statusText string
		if queuePosition > 1 {
			statusText = fmt.Sprintf("📋 当前排队位置: 第 %d 位", queuePosition)
		} else {
			statusText = "⚡ 即将开始处理"
		}
		text := b.formatLine(queuedTask, statusText, false)

		statusMsg := tgbotapi.NewMessage(message.Chat.ID, text)
		statusMsg.ReplyToMessageID = message.MessageID
		statusMsg.ReplyMarkup = keyboard
		sentMsg, err := b.api.Send(statusMsg)
		if err != nil {
			b.logger.Printf("发送消息失败: %v", err)
			return
		}

		queuedTask.StatusMsg = &sentMsg
		b.taskManager.EnqueueTask(queuedTask)
		return
	}

	// 如果没有 t.me 链接，检查是否是其他订阅链接 (http/https 但不是 t.me)
	re := regexp.MustCompile(`https?://[^\s]+`)
	if match := re.FindString(text); match != "" && !strings.Contains(match, "t.me") {
		b.logger.Printf("检测到订阅链接: %s", match)

		// 发送处理中消息
		statusMsg := tgbotapi.NewMessage(message.Chat.ID, "⏳ 正在添加订阅...")
		statusMsg.ReplyToMessageID = message.MessageID
		sentMsg, err := b.api.Send(statusMsg)
		if err != nil {
			b.logger.Printf("发送消息失败: %v", err)
			return
		}

		// 添加订阅
		_, responseMsg := b.addSubscription(match)

		// 更新消息
		editMsg := tgbotapi.NewEditMessageText(message.Chat.ID, sentMsg.MessageID, responseMsg)
		b.api.Send(editMsg)
		return
	}

	// 无效的消息（既不是 t.me 也不是订阅链接）
	b.logger.Printf("用户 %d 发送了无效消息", user.ID)
	warningMsg := tgbotapi.NewMessage(message.Chat.ID,
		"⚠️ 请发送以下类型的链接:\n"+
			"• Telegram 链接 (https://t.me/...)\n"+
			"• 订阅链接 (http/https 格式)")
	warningMsg.ReplyToMessageID = message.MessageID
	sentWarning, err := b.api.Send(warningMsg)
	if err != nil {
		return
	}

	// 异步删除提示和原消息(不阻塞主线程)
	go func() {
		time.Sleep(5 * time.Second)
		deleteMsg := tgbotapi.NewDeleteMessage(message.Chat.ID, sentWarning.MessageID)
		b.api.Send(deleteMsg)
		deleteOriginal := tgbotapi.NewDeleteMessage(message.Chat.ID, message.MessageID)
		b.api.Send(deleteOriginal)
	}()

}

// startQueueProcessor 启动队列处理器
func (b *Bot) startQueueProcessor() {
	b.taskManager.mu.Lock()
	if b.taskManager.queueProcessing {
		b.taskManager.mu.Unlock()
		return
	}
	b.taskManager.queueProcessing = true
	b.taskManager.mu.Unlock()

	b.logger.Println("📋 队列处理器已启动")

	go func() {
		for queuedTask := range b.taskManager.queue {
			b.logger.Printf("📤 从队列中取出任务 #%d (用户 %d), 剩余队列: %d",
				queuedTask.TaskID, queuedTask.UserID, b.taskManager.GetQueueSize())

			// 检查任务是否已被取消
			queuedTask.CancelMutex.Lock()
			cancelled := queuedTask.Cancelled
			queuedTask.CancelMutex.Unlock()

			if cancelled {
				b.logger.Printf("❌ 任务 #%d 已被取消，跳过执行", queuedTask.TaskID)
				b.taskManager.RemoveQueuedTask(queuedTask.UserID, queuedTask.TaskID)
				// 消息已在取消时更新，这里不需要再更新
				continue
			}

			// 更新消息状态为"处理中"
			if queuedTask.StatusMsg != nil {
				statusText := b.formatLine(queuedTask, "⏳ 已接收请求，处理中...", queuedTask.Shared)
				// 如果这是共享汇总消息，使用 updateSummaryLine 只替换对应行，保留其它行
				if queuedTask.Shared {
					b.updateSummaryLine(queuedTask.StatusMsg.Chat.ID, queuedTask.StatusMsg.MessageID, queuedTask.Index, statusText)
				} else {
					editMsg := tgbotapi.NewEditMessageText(
						queuedTask.StatusMsg.Chat.ID,
						queuedTask.StatusMsg.MessageID,
						statusText,
					)
					keyboard := tgbotapi.NewInlineKeyboardMarkup(
						tgbotapi.NewInlineKeyboardRow(
							tgbotapi.NewInlineKeyboardButtonData("🛑 终止任务",
								fmt.Sprintf("cancel_%d_%d", queuedTask.UserID, queuedTask.TaskID)),
						),
					)
					editMsg.ReplyMarkup = &keyboard
					b.api.Send(editMsg)
				}
			}

			// 执行任务
			b.processTDLForward(queuedTask)

			// 从队列任务映射中移除
			b.taskManager.RemoveQueuedTask(queuedTask.UserID, queuedTask.TaskID)

			b.logger.Printf("✅ 任务 #%d 处理完成 (用户 %d), 剩余队列: %d",
				queuedTask.TaskID, queuedTask.UserID, b.taskManager.GetQueueSize())
		}
	}()
}

// processTDLForward 执行 TDL 转发命令
func (b *Bot) processTDLForward(queuedTask *QueuedTask) {
	userID := queuedTask.UserID
	chatID := queuedTask.Message.Chat.ID
	taskID := queuedTask.TaskID
	link := queuedTask.Link
	sentMsg := queuedTask.StatusMsg

	// 创建任务
	task := &Task{
		UserID:  userID,
		ID:      taskID,
		Message: sentMsg,
	}

	// 添加任务到管理器（用于跟踪执行中的任务）
	b.taskManager.mu.Lock()
	if b.taskManager.tasks[userID] == nil {
		b.taskManager.tasks[userID] = make(map[int]*Task)
	}
	b.taskManager.tasks[userID][taskID] = task
	b.taskManager.mu.Unlock()

	// 设置为当前任务
	b.taskManager.SetCurrentTask(task)

	defer func() {
		b.taskManager.RemoveTask(userID, taskID)
		b.taskManager.SetCurrentTask(nil)
	}()

	b.logger.Printf("开始处理用户 %d 的转发请求 (任务 #%d)", userID, taskID)

	// 创建终止按钮
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🛑 终止任务", fmt.Sprintf("cancel_%d_%d", userID, taskID)),
		),
	)

	// 创建上下文用于取消
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	task.Cancel = cancel

	// 构建命令
	taskLockID := fmt.Sprintf("%d_%d", userID, taskID)
	cmd := exec.CommandContext(ctx, "bash", TDLScriptPath, link, taskLockID)
	// 尝试为子进程设置进程组
	setProcessGroup(cmd)
	task.Cmd = cmd

	// 创建管道读取输出
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		b.logger.Printf("创建管道失败: %v", err)
		if queuedTask.Shared {
			b.updateSummaryLine(chatID, sentMsg.MessageID, queuedTask.Index, b.formatSummaryLine(queuedTask, fmt.Sprintf("❌ 任务 #%d 启动失败", taskID)))
		} else {
			b.updateTaskMessage(chatID, sentMsg.MessageID, fmt.Sprintf("❌ 任务 #%d 启动失败", taskID), nil)
		}
		return
	}

	// 启动命令
	if err := cmd.Start(); err != nil {
		b.logger.Printf("启动命令失败: %v", err)
		if queuedTask.Shared {
			b.updateSummaryLine(chatID, sentMsg.MessageID, queuedTask.Index, b.formatSummaryLine(queuedTask, fmt.Sprintf("❌ 任务 #%d 启动失败", taskID)))
		} else {
			b.updateTaskMessage(chatID, sentMsg.MessageID, fmt.Sprintf("❌ 任务 #%d 启动失败", taskID), nil)
		}
		return
	}

	// 记录进程组 ID，便于后续取消/清理（如果可用）
	if cmd.Process != nil {
		if pgid, e := syscall.Getpgid(cmd.Process.Pid); e == nil {
			task.PGID = pgid
		}
	}

	// 读取输出
	lastUpdate := time.Now()
	currentStatus := fmt.Sprintf("⏳ 任务 #%d - 已接收请求，处理中...", taskID)
	qrDetected := false
	statusSeen := false

	lineChan := make(chan string)
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Split(bufio.ScanLines)
		for scanner.Scan() {
			lineChan <- scanner.Text()
		}
		close(lineChan)
	}()

	// 处理输出
	for line := range lineChan {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		b.logger.Printf("TDL 输出 (任务 #%d): %s", taskID, line)

		// 检测二维码 ASCII 字符
		if !qrDetected && (strings.Contains(line, "Scan QR code") || strings.Contains(line, "█")) {
			qrDetected = true
			b.logger.Printf("检测到登录二维码 (任务 #%d)", taskID)

			qrMessage := fmt.Sprintf(
				"🔐 任务 #%d - 需要登录\n\n"+
					"📺 请到服务器控制台查看二维码并使用 Telegram 扫描登录\n\n"+
					"⏰ 登录后任务将自动继续",
				taskID,
			)
			b.updateTaskMessage(chatID, sentMsg.MessageID, qrMessage, &keyboard)
			continue
		}

		// 检测二维码链接 (如果 tdl.sh 成功提取了链接)
		if strings.Contains(line, "[QRCODE]") {
			qrLink := strings.ReplaceAll(line, "[QRCODE]", "")
			qrLink = strings.TrimSpace(qrLink)
			b.logger.Printf("检测到登录二维码链接: %s", qrLink)

			qrMessage := fmt.Sprintf(
				"🔐 任务 #%d - 需要登录\n\n"+
					"📱 请点击以下链接在 Telegram 中完成登录:\n"+
					"%s\n\n"+
					"⏰ 登录后任务将自动继续",
				taskID, qrLink,
			)
			b.updateTaskMessage(chatID, sentMsg.MessageID, qrMessage, &keyboard)
			continue
		} // 只处理带 [STATUS] 标记的消息
		if strings.Contains(line, "[STATUS]") {
			cleanLine := strings.ReplaceAll(line, "[STATUS]", "")
			cleanLine = strings.TrimSpace(cleanLine)
			currentStatus = cleanLine
			statusSeen = true

			// 限制更新频率 (至少间隔1秒)
			if time.Since(lastUpdate) >= time.Second {
				if queuedTask.Shared {
					b.updateSummaryLine(chatID, sentMsg.MessageID, queuedTask.Index, b.formatLine(queuedTask, currentStatus, true))
				} else {
					b.updateTaskMessage(chatID, sentMsg.MessageID, b.formatLine(queuedTask, currentStatus, false), &keyboard)
				}
				lastUpdate = time.Now()
			}
		}

		// 检查任务是否被取消
		if _, exists := b.taskManager.GetTask(userID, taskID); !exists {
			b.logger.Printf("用户 %d 的任务 #%d 已被取消", userID, taskID)
			if queuedTask.Shared {
				b.updateSummaryLine(chatID, sentMsg.MessageID, queuedTask.Index, b.formatSummaryLine(queuedTask, fmt.Sprintf("❌ 任务 #%d 已被用户终止", taskID)))
				// 任务在运行中被取消：递减汇总待完成计数并在必要时清除键盘
				if remaining := b.taskManager.DecrementSummaryPending(chatID, sentMsg.MessageID); remaining <= 0 {
					b.clearSummaryKeyboard(chatID, sentMsg.MessageID)
				}
			} else {
				b.updateTaskMessage(chatID, sentMsg.MessageID, fmt.Sprintf("❌ 任务 #%d 已被用户终止", taskID), nil)
			}
			return
		}
	}

	// 等待命令完成
	err = cmd.Wait()

	// 任务结束后，尝试清理残留的进程组（如果存在的话）：先 SIGTERM，再短等待，再 SIGKILL
	if task.PGID == 0 {
		if cmd.Process != nil {
			// 尝试获取 PGID
			if pgid, e := syscall.Getpgid(cmd.Process.Pid); e == nil {
				task.PGID = pgid
			}
		}
	}
	if task.PGID != 0 {
		// 先尝试温和终止
		_ = syscall.Kill(-task.PGID, syscall.SIGTERM)
		time.Sleep(800 * time.Millisecond)
		// 再强制杀掉
		_ = syscall.Kill(-task.PGID, syscall.SIGKILL)
	}

	b.logger.Printf("TDL 脚本执行完成 (任务 #%d), 错误: %v", taskID, err)

	// 根据返回结果更新最终状态
	var finalStatus string
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			finalStatus = fmt.Sprintf("❌ 任务 #%d 执行超时", taskID)
		} else {
			finalStatus = fmt.Sprintf("⚠️ 任务 #%d 执行失败", taskID)
		}
	} else {
		// 如果曾经接收到过 [STATUS] 行，优先使用最后一条非链接的 status 文本作为最终状态
		if statusSeen && strings.TrimSpace(currentStatus) != "" {
			finalStatus = strings.ReplaceAll(currentStatus, "[STATUS]", "")
			finalStatus = strings.TrimSpace(finalStatus)
		} else {
			finalStatus = fmt.Sprintf("✅ 任务 #%d 处理完成", taskID)
		}
	}

	// 更新为最终状态(移除按钮)
	if queuedTask.Shared {
		b.updateSummaryLine(chatID, sentMsg.MessageID, queuedTask.Index, b.formatSummaryDoneLine(queuedTask, finalStatus))
		// 递减待完成计数；如果这是最后一项，清除汇总上的键盘按钮
		if remaining := b.taskManager.DecrementSummaryPending(chatID, sentMsg.MessageID); remaining <= 0 {
			b.clearSummaryKeyboard(chatID, sentMsg.MessageID)
		}
	} else {
		// 单条任务也应以单行显示最终状态
		b.updateTaskMessage(chatID, sentMsg.MessageID, b.formatTaskDoneLine(queuedTask, finalStatus), nil)
	}
}

// handleCallbackQuery 处理回调查询 (按钮点击)
func (b *Bot) handleCallbackQuery(query *tgbotapi.CallbackQuery) {
	// 解析回调数据: 支持 cancel_summary_<userID> 和 cancel_<userID>_<taskID>
	if !strings.HasPrefix(query.Data, "cancel_") {
		return
	}

	parts := strings.Split(query.Data, "_")
	if len(parts) < 2 {
		callback := tgbotapi.NewCallback(query.ID, "⚠️ 无效的任务标识")
		callback.ShowAlert = true
		b.api.Request(callback)
		return
	}

	currentUserID := query.From.ID

	// 处理汇总取消: cancel_summary_<userID>
	if parts[1] == "summary" {
		if len(parts) < 3 {
			callback := tgbotapi.NewCallback(query.ID, "⚠️ 无效的任务标识")
			callback.ShowAlert = true
			b.api.Request(callback)
			return
		}
		var targetUserID int64
		fmt.Sscanf(parts[2], "%d", &targetUserID)

		// 验证权限 (只能取消自己的汇总)
		if currentUserID != targetUserID {
			callback := tgbotapi.NewCallback(query.ID, "❌ 您无权终止此任务汇总")
			callback.ShowAlert = true
			b.api.Request(callback)
			return
		}

		// 取消该汇总消息下的所有队列任务
		if query.Message != nil {
			// 先处理队列中的任务
			b.taskManager.mu.RLock()
			queuedMap := b.taskManager.queuedTasks[targetUserID]
			b.taskManager.mu.RUnlock()
			if queuedMap != nil {
				for _, q := range queuedMap {
					if q.StatusMsg != nil && q.StatusMsg.MessageID == query.Message.MessageID {
						b.taskManager.CancelQueuedTask(targetUserID, q.TaskID)
						b.updateSummaryLine(q.StatusMsg.Chat.ID, q.StatusMsg.MessageID, q.Index, b.formatSummaryLine(q, fmt.Sprintf("❌ 任务 #%d 已从汇总取消", q.TaskID)))
						// 取消队列中的任务后应递减汇总待完成计数并在必要时清除键盘
						if remaining := b.taskManager.DecrementSummaryPending(q.StatusMsg.Chat.ID, q.StatusMsg.MessageID); remaining <= 0 {
							b.clearSummaryKeyboard(q.StatusMsg.Chat.ID, q.StatusMsg.MessageID)
						}
					}
				}
			}

			// 再尝试取消正在执行的任务（若其关联到同一条汇总消息）
			b.taskManager.mu.RLock()
			running := b.taskManager.tasks[targetUserID]
			b.taskManager.mu.RUnlock()
			if running != nil {
				for tid, t := range running {
					if t.Message != nil && t.Message.MessageID == query.Message.MessageID {
						b.taskManager.CancelTask(targetUserID, tid)
						// 尝试查找对应的 queued 以更新汇总行（若仍存在）
						if q, ok := b.taskManager.GetQueuedTask(targetUserID, tid); ok {
							b.updateSummaryLine(query.Message.Chat.ID, query.Message.MessageID, q.Index, b.formatSummaryLine(q, fmt.Sprintf("❌ 任务 #%d 已被用户终止", tid)))
							// 对于仍在 queued 映射中的项，我们需要递减汇总计数
							if remaining := b.taskManager.DecrementSummaryPending(query.Message.Chat.ID, query.Message.MessageID); remaining <= 0 {
								b.clearSummaryKeyboard(query.Message.Chat.ID, query.Message.MessageID)
							}
						}
					}
				}
			}
		}

		callback := tgbotapi.NewCallback(query.ID, "")
		b.api.Request(callback)
		return
	}

	// 非汇总取消，解析 cancel_<userID>_<taskID>
	if len(parts) < 3 {
		callback := tgbotapi.NewCallback(query.ID, "⚠️ 无效的任务标识")
		callback.ShowAlert = true
		b.api.Request(callback)
		return
	}
	var targetUserID, taskID int64
	fmt.Sscanf(parts[1], "%d", &targetUserID)
	fmt.Sscanf(parts[2], "%d", &taskID)

	// 验证权限 (只能取消自己的任务)
	if currentUserID != targetUserID {
		callback := tgbotapi.NewCallback(query.ID, "❌ 您无权终止此任务")
		callback.ShowAlert = true
		b.api.Request(callback)
		return
	}

	// 先尝试取消队列中的任务
	if queued, ok := b.taskManager.GetQueuedTask(targetUserID, int(taskID)); ok {
		// 标记取消
		cancelled := b.taskManager.CancelQueuedTask(targetUserID, int(taskID))
		if cancelled {
			b.logger.Printf("用户 %d 取消了队列中的任务 #%d", targetUserID, taskID)
			// 如果是共享汇总消息，只更新对应行
			if queued.Shared && queued.StatusMsg != nil {
				b.updateSummaryLine(queued.StatusMsg.Chat.ID, queued.StatusMsg.MessageID, queued.Index, b.formatSummaryLine(queued, fmt.Sprintf("❌ 任务 #%d 已从队列中取消", taskID)))
				// 递减汇总待完成计数并在必要时清除键盘
				if remaining := b.taskManager.DecrementSummaryPending(queued.StatusMsg.Chat.ID, queued.StatusMsg.MessageID); remaining <= 0 {
					b.clearSummaryKeyboard(queued.StatusMsg.Chat.ID, queued.StatusMsg.MessageID)
				}
			} else if queued.StatusMsg != nil {
				// 非共享，直接替换整条状态消息
				editMsg := tgbotapi.NewEditMessageText(
					query.Message.Chat.ID,
					queued.StatusMsg.MessageID,
					fmt.Sprintf("❌ 任务 #%d 已从队列中取消", taskID),
				)
				b.api.Send(editMsg)
			}

			callback := tgbotapi.NewCallback(query.ID, "")
			b.api.Request(callback)
			return
		}
	}

	// 如果不在队列中，尝试终止正在执行的任务
	if b.taskManager.CancelTask(targetUserID, int(taskID)) {
		b.logger.Printf("用户 %d 终止了执行中的任务 #%d", targetUserID, taskID)

		// 如果该消息是汇总消息，更新对应行；否则替换整条
		if query.Message != nil {
			if linesMap, ok := b.taskManager.summaryLines[query.Message.Chat.ID]; ok {
				if _, ok2 := linesMap[query.Message.MessageID]; ok2 {
					// 找 queued task to find index
					if queued, ok3 := b.taskManager.GetQueuedTask(targetUserID, int(taskID)); ok3 && queued.Shared {
						b.updateSummaryLine(query.Message.Chat.ID, query.Message.MessageID, queued.Index, b.formatSummaryDoneLine(queued, fmt.Sprintf("❌ 任务 #%d 已被用户终止", taskID)))
					} else {
						editMsg := tgbotapi.NewEditMessageText(
							query.Message.Chat.ID,
							query.Message.MessageID,
							fmt.Sprintf("❌ 任务 #%d 已被用户终止", taskID),
						)
						b.api.Send(editMsg)
					}
				} else {
					editMsg := tgbotapi.NewEditMessageText(
						query.Message.Chat.ID,
						query.Message.MessageID,
						fmt.Sprintf("❌ 任务 #%d 已被用户终止", taskID),
					)
					b.api.Send(editMsg)
				}
			} else {
				editMsg := tgbotapi.NewEditMessageText(
					query.Message.Chat.ID,
					query.Message.MessageID,
					fmt.Sprintf("❌ 任务 #%d 已被用户终止", taskID),
				)
				b.api.Send(editMsg)
			}
		}

		callback := tgbotapi.NewCallback(query.ID, "")
		b.api.Request(callback)
	} else {
		callback := tgbotapi.NewCallback(query.ID, "⚠️ 任务已完成或不存在")
		callback.ShowAlert = true
		b.api.Request(callback)
	}
}

// updateTaskMessage 更新任务消息
func (b *Bot) updateTaskMessage(chatID int64, messageID int, text string, keyboard *tgbotapi.InlineKeyboardMarkup) {
	editMsg := tgbotapi.NewEditMessageText(chatID, messageID, text)
	if keyboard != nil {
		editMsg.ReplyMarkup = keyboard
	}
	_, err := b.api.Send(editMsg)
	if err != nil {
		b.logger.Printf("更新消息失败: %v", err)
	}
}

// updateSummaryLine 更新汇总消息的特定行（并一次性编辑整条消息）
func (b *Bot) updateSummaryLine(chatID int64, messageID int, index int, text string) {
	// 更新缓存并获取最新行列表
	lines, kb := b.taskManager.UpdateSummaryLine(chatID, messageID, index, text)
	if lines == nil {
		// 没有缓存，退回为直接替换整条
		edit := tgbotapi.NewEditMessageText(chatID, messageID, text)
		_, err := b.api.Send(edit)
		if err != nil {
			b.logger.Printf("更新汇总消息失败: %v", err)
		}
		return
	}

	// 重新拼接整条消息文本并保留键盘（如果存在）
	full := strings.Join(lines, "\n\n")
	edit := tgbotapi.NewEditMessageText(chatID, messageID, full)
	if kb != nil {
		edit.ReplyMarkup = kb
	}
	_, err := b.api.Send(edit)
	if err != nil {
		b.logger.Printf("更新汇总消息失败: %v", err)
	}
}

// clearSummaryKeyboard 从汇总消息中移除内联键盘（不修改文本）
func (b *Bot) clearSummaryKeyboard(chatID int64, messageID int) {
	lines, ok := b.taskManager.GetSummaryLines(chatID, messageID)
	if !ok || len(lines) == 0 {
		return
	}
	full := strings.Join(lines, "\n\n")
	edit := tgbotapi.NewEditMessageText(chatID, messageID, full)
	// 不设置 ReplyMarkup，即移除键盘
	_, err := b.api.Send(edit)
	if err != nil {
		b.logger.Printf("清除汇总键盘失败: %v", err)
	}
}

// formatSummaryLine 将任务状态格式化为单行用于汇总消息
func (b *Bot) formatSummaryLine(q *QueuedTask, status string) string {
	return b.formatLine(q, status, true)
}

// formatSummaryDoneLine 返回用于汇总在任务完成或失败时显示的单行文本，
// 形如: "📌 任务 #1 ✅ 转发完成"。会尝试去掉 final 中重复的 "任务 #N" 前缀并压缩为单行。
func (b *Bot) formatSummaryDoneLine(q *QueuedTask, final string) string {
	// 先把多行合并为一行并修剪
	s := strings.ReplaceAll(final, "\n", " ")
	s = strings.TrimSpace(s)

	// 尝试移除 final 中可能已经包含的 "任务 #<id>" 子串，避免重复
	targ := fmt.Sprintf("任务 #%d", q.TaskID)
	if idx := strings.Index(s, targ); idx != -1 {
		s = strings.TrimSpace(s[idx+len(targ):])
	}
	// 去除前导符号
	s = strings.TrimLeft(s, " -–—:：")
	s = strings.TrimSpace(s)
	if s == "" {
		s = "已完成"
	}

	// 使用统一单行格式
	return b.formatLine(q, s, true)
}

// formatTaskDoneLine 为单条任务生成单行的最终显示文本
func (b *Bot) formatTaskDoneLine(q *QueuedTask, final string) string {
	// 使用统一单行格式（不带序号）
	s := strings.ReplaceAll(final, "\n", " ")
	s = strings.TrimSpace(s)
	return b.formatLine(q, s, false)
}

// formatLine 生成统一的一行显示: 可选前缀序号, 然后 [#taskID] <link> — <status>
func (b *Bot) formatLine(q *QueuedTask, status string, includeIndex bool) string {
	// 先清理 status 中可能包含的 "任务 #<id>" 前缀，避免重复显示任务编号
	s := status
	targ := fmt.Sprintf("任务 #%d", q.TaskID)
	if idx := strings.Index(s, targ); idx != -1 {
		// 仅移除位于开头或开头附近的前缀
		// 找到 targ 后移除并去除常见分隔符
		after := strings.TrimSpace(s[idx+len(targ):])
		after = strings.TrimLeft(after, " -–—:：")
		s = strings.TrimSpace(after)
	}

	// 选择一行进度文本：优先取 s 的最后一个非空、非链接行
	progress := ""
	parts := strings.Split(s, "\n")
	for i := len(parts) - 1; i >= 0; i-- {
		l := strings.TrimSpace(parts[i])
		if l == "" {
			continue
		}
		if strings.Contains(l, "http") || strings.Contains(strings.ToLower(l), "t.me") {
			continue
		}
		progress = l
		break
	}
	if progress == "" {
		// 回退到清理后的字符串 s，优先取第一行非链接的文本
		for _, p := range parts {
			pp := strings.TrimSpace(p)
			if pp == "" {
				continue
			}
			if strings.Contains(pp, "http") || strings.Contains(strings.ToLower(pp), "t.me") {
				continue
			}
			progress = pp
			break
		}
		if progress == "" {
			progress = strings.SplitN(s, "\n", 2)[0]
		}
	}
	if len(progress) > 200 {
		progress = progress[:200] + "..."
	}

	base := fmt.Sprintf("[#%d] %s — %s", q.TaskID, q.Link, progress)
	if includeIndex {
		return fmt.Sprintf("%d. %s", q.Index+1, base)
	}
	return base
}

// Run 启动 Bot
func (b *Bot) Run() error {
	b.logger.Println("=" + strings.Repeat("=", 49))
	b.logger.Println("正在启动 Telegram TDL Bot...")
	b.logger.Printf("TDL 脚本路径: %s", TDLScriptPath)
	if AllowedUsers == nil {
		b.logger.Println("权限模式: 开放")
	} else {
		b.logger.Println("权限模式: 白名单")
	}
	b.logger.Println("任务模式: 排队执行 (一次一个)")
	b.logger.Println("=" + strings.Repeat("=", 49))

	// 检查 TDL 脚本是否存在
	if _, err := os.Stat(TDLScriptPath); os.IsNotExist(err) {
		b.logger.Printf("❌ TDL 脚本未找到: %s", TDLScriptPath)
		b.logger.Println("请检查 TDL_SCRIPT_PATH 配置或脚本路径")
		return fmt.Errorf("TDL 脚本未找到")
	}

	// 启动队列处理器
	b.startQueueProcessor()

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates := b.api.GetUpdatesChan(u)

	b.logger.Printf("✅ Bot 已启动 (@%s), 按 Ctrl-C 停止", b.api.Self.UserName)
	b.logger.Println("等待接收消息...")

	// 处理信号
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	for {
		select {
		case <-sigChan:
			b.logger.Println("收到停止信号，正在关闭...")
			b.api.StopReceivingUpdates()
			return nil

		case update := <-updates:
			if update.Message != nil {
				// 处理命令
				if update.Message.IsCommand() {
					switch update.Message.Command() {
					case "start":
						b.handleStart(update.Message)
					case "help":
						b.handleHelp(update.Message)
					case "status":
						b.handleStatus(update.Message)
					default:
						msg := tgbotapi.NewMessage(update.Message.Chat.ID, "❓ 未知命令，使用 /help 查看帮助")
						msg.ReplyToMessageID = update.Message.MessageID
						b.api.Send(msg)
					}
				} else if update.Message.Text != "" {
					// 处理普通文本消息
					b.handleMessage(update.Message)
				}
			} else if update.CallbackQuery != nil {
				// 处理回调查询
				b.handleCallbackQuery(update.CallbackQuery)
			}
		}
	}
}

// truncateString 截断字符串
func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// setProcessGroup 为子进程设置新的进程组 (仅 Unix/Linux)
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup 通过向负 PID 发送信号终止整个进程组 (仅 Unix/Linux)
func killProcessGroup(pid int) error {
	// 向负 PID 发送信号以作用于进程组
	return syscall.Kill(-pid, syscall.SIGKILL)
}

func main() {
	// 优先使用环境变量指定的脚本路径
	TDLScriptPath = os.Getenv("TDL_SCRIPT_PATH")

	if TDLScriptPath == "" {
		// 如果没有环境变量，尝试获取可执行文件所在目录
		exePath, err := os.Executable()
		if err == nil {
			exePath, _ = filepath.EvalSymlinks(exePath)
			exeDir := filepath.Dir(exePath)
			TDLScriptPath = filepath.Join(exeDir, "tdl.sh")
		}

		// 如果可执行文件路径看起来是临时目录（包含 go-build），使用当前工作目录
		if strings.Contains(TDLScriptPath, "go-build") || TDLScriptPath == "" {
			workDir, err := os.Getwd()
			if err == nil {
				TDLScriptPath = filepath.Join(workDir, "tdl.sh")
			}
		}
	}

	// 如果脚本仍然不存在，尝试常用路径
	if _, err := os.Stat(TDLScriptPath); os.IsNotExist(err) {
		// 尝试当前目录
		if workDir, err := os.Getwd(); err == nil {
			testPath := filepath.Join(workDir, "tdl.sh")
			if _, err := os.Stat(testPath); err == nil {
				TDLScriptPath = testPath
			}
		}
	}

	bot, err := NewBot(BotToken)
	if err != nil {
		log.Fatalf("创建 Bot 失败: %v", err)
	}

	if err := bot.Run(); err != nil {
		log.Fatalf("Bot 运行失败: %v", err)
	}
}
