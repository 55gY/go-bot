# Telegram 消息转发机器人

**独立运行的 Go 语言 Telegram Bot，无需依赖 tdl**

[![GitHub](https://img.shields.io/badge/GitHub-55gY%2Fgo--bot-blue)](https://github.com/55gY/go-bot)

## 📦 项目简介

`go-bot` 是一个独立运行的 Telegram 机器人，用于转发频道/群组消息。

### ✨ 核心特性

- ❌ **不依赖 tdl** - 完全独立运行
- 🔄 **队列处理** - 任务按顺序执行，避免并发冲突
- 📱 **单消息生命周期** - 从排队到完成全程一条消息更新
- 🛑 **任务取消** - 支持中断正在执行或排队中的任务
- 🔐 **自动登录检测** - 检测到未登录时提示扫码
- 📊 **实时状态** - 任务进度实时更新
- 🎯 **订阅验证** - 集成订阅 API 验证用户权限

## 🔗 相关项目

| 项目 | 说明 | 依赖 tdl | Session 数量 | GitHub |
|------|------|----------|--------------|--------|
| **go-bot** (本项目) | 独立转发机器人 | ❌ | 1 | [![GitHub](https://img.shields.io/badge/GitHub-repo-blue)](https://github.com/55gY/go-bot) |
| [go-TelegramMessage](https://github.com/55gY/go-TelegramMessage) | 独立消息监听器 | ❌ | 1 | [![GitHub](https://img.shields.io/badge/GitHub-repo-blue)](https://github.com/55gY/go-TelegramMessage) |
| [tdl-msgproce](https://github.com/55gY/tdl-msgproce) | 基于 tdl 的融合版 | ✅ | 1 | [![GitHub](https://img.shields.io/badge/GitHub-repo-blue)](https://github.com/55gY/tdl-msgproce) |

### 📊 项目选择指南

- **需要转发功能但不想安装 tdl**：使用本项目（go-bot）
- **需要监听+转发，且已有 tdl**：推荐 [tdl-msgproce](https://github.com/55gY/tdl-msgproce)
- **只需要消息监听**：使用 [go-TelegramMessage](https://github.com/55gY/go-TelegramMessage)

## 📋 环境要求

- Go 1.16+
- Bash（Linux/Mac）或 PowerShell（Windows）
- **不需要** tdl（本项目通过 tdl.sh 脚本独立运行）

## 🚀 快速开始

### 方法一：使用管理脚本（推荐）

```bash
# 交互式菜单
bash setup.sh

# 或直接执行命令
bash setup.sh check      # 检查依赖
bash setup.sh status     # 服务状态
bash setup.sh install    # 安装服务
bash setup.sh start      # 控制台启动
```

### 方法二：手动编译

```bash
# 克隆仓库
git clone https://github.com/55gY/go-bot.git
cd go-bot

# 编译
go mod tidy
go build -o tgbot tgbot.go

# 运行
./tgbot
```

## ⚙️ 配置

编辑 `tgbot.go` 文件：

```go
const (
    BotToken = "YOUR_BOT_TOKEN"  // 从 @BotFather 获取
    SubscriptionAPIHost = "YOUR_API_HOST:PORT"  // 订阅验证 API（可选）
)
```

## 📖 使用说明

### 与机器人对话

1. 发送 Telegram 频道/群组链接
2. Bot 验证订阅状态（如配置了 API）
3. 任务加入队列等待处理
4. 实时显示任务进度
5. 可随时点击"🛑 终止任务"取消

### 命令列表

- `/start` - 启动机器人
- `/help` - 查看帮助
- `/cancel` - 取消当前任务
- 直接发送链接 - 开始转发任务

### 支持的链接格式

- `https://t.me/channel_name`
- `https://t.me/c/1234567890`
- `https://t.me/joinchat/xxxxx`

## 🔧 管理脚本功能

`setup.sh` 提供以下功能：

1. **检查环境依赖** - 查看 Go、Bash、TDL 等依赖安装状态
2. **检查后台服务** - 查看服务运行状态、日志
3. **安装后台服务** - 自动编译、安装、启动 systemd 服务
4. **控制台启动** - 前台调试模式运行
5. **卸载服务** - 停止并删除 systemd 服务
6. **查看实时日志** - 实时跟踪服务日志

## 🎛️ 服务管理

### 安装为系统服务

```bash
sudo bash setup.sh
# 选择: 3. 安装/更新后台服务
```

### 服务控制命令

```bash
# 启动服务
sudo systemctl start tgbot-go

# 停止服务
sudo systemctl stop tgbot-go

# 重启服务
sudo systemctl restart tgbot-go

# 查看状态
sudo systemctl status tgbot-go

# 查看日志
sudo journalctl -u tgbot-go -f

# 开机自启
sudo systemctl enable tgbot-go

# 禁用自启
sudo systemctl disable tgbot-go
```

## 📁 文件结构

```
go-bot/
├── tgbot.go           # 主程序
├── setup.sh           # 管理脚本
├── tdl.sh             # TDL 包装脚本（独立于 tdl 安装）
├── go.mod             # Go 模块定义
├── go.sum             # 依赖校验
├── default            # 转发列表文件
└── README.md          # 说明文档
```

## 🔐 登录说明

首次使用需要登录 Telegram 账号：

1. Bot 检测到未登录时会提示：
   ```
   🔐 任务 #1 - 需要登录
   📺 请到服务器控制台查看二维码并使用 Telegram 扫描登录
   ```

2. 到服务器控制台查看二维码（tdl.sh 会自动调用 tdl 登录）
3. 使用 Telegram 手机客户端扫码登录
4. 登录成功后任务自动继续

**注意**：本项目通过 tdl.sh 脚本独立管理 tdl，无需预先安装 tdl。

## ⚙️ 高级配置

### 修改队列容量

编辑 `tgbot.go`：

```go
queue: make(chan *QueuedTask, 100),  // 队列容量
```

### 修改超时时间

```go
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
```

### TDL 数据目录

```
.tdl/
├── tdl              # TDL 可执行文件
└── data/            # 登录会话数据
    └── default/
```

## 🐛 故障排查

### 1. 服务无法启动

```bash
# 查看详细日志
sudo journalctl -u tgbot-go -n 50

# 检查端口占用
ss -tulnp | grep tgbot
```

### 2. 编译失败

```bash
# 更新依赖
go mod tidy

# 清理缓存
go clean -cache
```

### 3. 二维码显示不完整

确保终端支持 UTF-8 编码：

```bash
export LANG=zh_CN.UTF-8
```

## 📝 更新日志

### v2.0
- ✨ 新增队列处理机制
- ✨ 单消息生命周期
- ✨ 任务取消支持
- ✨ 动态路径适配
- ✨ 简化二维码登录
- 🔧 优化输出缓冲处理

### v1.0
- 🎉 初始版本发布

## 📄 开源协议

MIT License

## 🔗 相关链接

- **tdl-msgproce**: https://github.com/55gY/tdl-msgproce - 基于 tdl 的融合版（推荐）
- **go-TelegramMessage**: https://github.com/55gY/go-TelegramMessage - 纯 Go 消息监听器
- **TDL**: https://github.com/iyear/tdl - Telegram Downloader

## 💬 支持

遇到问题或有建议？欢迎提交 Issue！
