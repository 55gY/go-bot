#!/bin/bash

# Go Telegram Bot 一键安装管理脚本

set -e

GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
BLUE='\033[0;34m'
NC='\033[0m'

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SERVICE_NAME="tgbot-go"
BINARY_NAME="tgbot"

# 检查环境依赖
check_dependencies() {
    echo -e "${BLUE}======================================"
    echo "检查环境依赖"
    echo "======================================${NC}"
    echo ""
    
    local all_ok=true
    
    # 检查 Go
    if command -v go &> /dev/null; then
        GO_VERSION=$(go version | awk '{print $3}')
        echo -e "✅ Go: ${GREEN}已安装${NC} ($GO_VERSION)"
    else
        echo -e "❌ Go: ${RED}未安装${NC}"
        all_ok=false
    fi
    
    # 检查 Bash
    if command -v bash &> /dev/null; then
        BASH_VERSION=$(bash --version | head -n1)
        echo -e "✅ Bash: ${GREEN}已安装${NC}"
    else
        echo -e "❌ Bash: ${RED}未安装${NC}"
        all_ok=false
    fi
    
    # 检查 systemctl
    if command -v systemctl &> /dev/null; then
        echo -e "✅ systemd: ${GREEN}已安装${NC}"
    else
        echo -e "⚠️  systemd: ${YELLOW}未安装${NC} (后台服务功能不可用)"
    fi
    
    # 检查 TDL
    TDL_PATH="${SCRIPT_DIR}/.tdl/tdl"
    if [ -f "$TDL_PATH" ]; then
        echo -e "✅ TDL: ${GREEN}已安装${NC} ($TDL_PATH)"
    else
        echo -e "⚠️  TDL: ${YELLOW}未安装${NC} (将在首次运行时自动安装)"
    fi
    
    # 检查编译的二进制文件
    if [ -f "${SCRIPT_DIR}/${BINARY_NAME}" ]; then
        echo -e "✅ Bot程序: ${GREEN}已编译${NC}"
    else
        echo -e "⚠️  Bot程序: ${YELLOW}未编译${NC}"
    fi
    
    echo ""
    if [ "$all_ok" = true ]; then
        echo -e "${GREEN}所有必需依赖已安装${NC}"
    else
        echo -e "${RED}缺少必需依赖,请先安装${NC}"
    fi
    
    echo ""
}

# 检查服务状态
check_service() {
    echo -e "${BLUE}======================================"
    echo "检查后台服务状态"
    echo "======================================${NC}"
    echo ""
    
    if ! command -v systemctl &> /dev/null; then
        echo -e "${YELLOW}systemd 不可用${NC}"
        echo ""
        return
    fi
    
    if systemctl list-unit-files | grep -q "^${SERVICE_NAME}.service"; then
        echo -e "服务文件: ${GREEN}已安装${NC}"
        
        if systemctl is-active --quiet "${SERVICE_NAME}"; then
            echo -e "运行状态: ${GREEN}运行中${NC}"
        else
            echo -e "运行状态: ${RED}未运行${NC}"
        fi
        
        if systemctl is-enabled --quiet "${SERVICE_NAME}"; then
            echo -e "开机自启: ${GREEN}已启用${NC}"
        else
            echo -e "开机自启: ${YELLOW}未启用${NC}"
        fi
        
        echo ""
        echo "最近日志:"
        journalctl -u "${SERVICE_NAME}" -n 5 --no-pager 2>/dev/null || echo "无日志"
    else
        echo -e "服务文件: ${RED}未安装${NC}"
    fi
    
    echo ""
}

# 编译程序
compile_program() {
    echo -e "${YELLOW}编译 Go 程序...${NC}"
    cd "${SCRIPT_DIR}"
    
    if [ ! -f "tgbot.go" ]; then
        echo -e "${RED}错误: 找不到 tgbot.go 文件${NC}"
        return 1
    fi
    
    go build -o "${BINARY_NAME}" tgbot.go
    chmod +x "${BINARY_NAME}"
    echo -e "${GREEN}✅ 编译完成${NC}"
    echo ""
}

# 安装/更新后台服务
install_service() {
    echo -e "${BLUE}======================================"
    echo "安装后台服务"
    echo "======================================${NC}"
    echo ""
    
    # 检查 root 权限
    if [ "$EUID" -ne 0 ]; then 
        echo -e "${RED}需要 root 权限安装服务${NC}"
        echo "请使用: sudo bash $0"
        return 1
    fi
    
    # 检查 systemd
    if ! command -v systemctl &> /dev/null; then
        echo -e "${RED}systemd 不可用,无法安装服务${NC}"
        return 1
    fi
    
    # 编译程序
    compile_program
    
    # 停止旧服务
    if systemctl is-active --quiet "${SERVICE_NAME}"; then
        echo -e "${YELLOW}停止旧服务...${NC}"
        systemctl stop "${SERVICE_NAME}"
    fi
    
    # 创建服务文件
    echo -e "${YELLOW}创建服务文件...${NC}"
    SERVICE_FILE="/etc/systemd/system/${SERVICE_NAME}.service"
    
    cat > "$SERVICE_FILE" << EOF
[Unit]
Description=Telegram Bot Service (Go)
After=network.target network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root
WorkingDirectory=${SCRIPT_DIR}
ExecStart=${SCRIPT_DIR}/${BINARY_NAME}
ExecReload=/bin/kill -HUP \$MAINPID
KillMode=process
Restart=always
RestartSec=10
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF
    
    # 重新加载并启动服务
    echo -e "${YELLOW}启动服务...${NC}"
    systemctl daemon-reload
    systemctl enable "${SERVICE_NAME}"
    systemctl start "${SERVICE_NAME}"
    
    sleep 2
    
    if systemctl is-active --quiet "${SERVICE_NAME}"; then
        echo ""
        echo -e "${GREEN}✅ 服务安装成功并已启动${NC}"
        echo ""
        echo "服务管理命令:"
        echo "  systemctl start ${SERVICE_NAME}    # 启动"
        echo "  systemctl stop ${SERVICE_NAME}     # 停止"
        echo "  systemctl restart ${SERVICE_NAME}  # 重启"
        echo "  systemctl status ${SERVICE_NAME}   # 状态"
        echo "  journalctl -u ${SERVICE_NAME} -f  # 日志"
    else
        echo ""
        echo -e "${RED}❌ 服务启动失败${NC}"
        echo "查看日志: journalctl -u ${SERVICE_NAME} -n 50"
    fi
    
    echo ""
}

# 控制台启动(调试模式)
console_start() {
    echo -e "${BLUE}======================================"
    echo "控制台启动 (调试模式)"
    echo "======================================${NC}"
    echo ""
    
    # 编译程序
    compile_program
    
    # 检查是否已有服务在运行
    if command -v systemctl &> /dev/null && systemctl is-active --quiet "${SERVICE_NAME}"; then
        echo -e "${YELLOW}⚠️  检测到后台服务正在运行${NC}"
        echo ""
        read -p "是否停止后台服务? (y/n): " -n 1 -r
        echo
        if [[ $REPLY =~ ^[Yy]$ ]]; then
            sudo systemctl stop "${SERVICE_NAME}"
            echo -e "${GREEN}后台服务已停止${NC}"
            echo ""
        else
            echo -e "${YELLOW}将继续启动,可能会有端口冲突${NC}"
            echo ""
        fi
    fi
    
    echo -e "${GREEN}启动程序...${NC}"
    echo -e "${YELLOW}按 Ctrl+C 退出${NC}"
    echo ""
    echo "----------------------------------------"
    
    cd "${SCRIPT_DIR}"
    "./${BINARY_NAME}"
}

# 卸载服务
uninstall_service() {
    echo -e "${BLUE}======================================"
    echo "卸载后台服务"
    echo "======================================${NC}"
    echo ""
    
    # 检查 root 权限
    if [ "$EUID" -ne 0 ]; then 
        echo -e "${RED}需要 root 权限卸载服务${NC}"
        echo "请使用: sudo bash $0"
        return 1
    fi
    
    if ! command -v systemctl &> /dev/null; then
        echo -e "${RED}systemd 不可用${NC}"
        return 1
    fi
    
    SERVICE_FILE="/etc/systemd/system/${SERVICE_NAME}.service"
    
    if [ ! -f "$SERVICE_FILE" ]; then
        echo -e "${YELLOW}服务未安装${NC}"
        return 0
    fi
    
    # 停止并禁用服务
    if systemctl is-active --quiet "${SERVICE_NAME}"; then
        echo -e "${YELLOW}停止服务...${NC}"
        systemctl stop "${SERVICE_NAME}"
    fi
    
    echo -e "${YELLOW}禁用服务...${NC}"
    systemctl disable "${SERVICE_NAME}" 2>/dev/null || true
    
    # 删除服务文件
    echo -e "${YELLOW}删除服务文件...${NC}"
    rm -f "$SERVICE_FILE"
    
    systemctl daemon-reload
    
    echo ""
    echo -e "${GREEN}✅ 服务已卸载${NC}"
    echo ""
}

# 显示主菜单
show_menu() {
    clear
    echo -e "${GREEN}======================================"
    echo "  Go Telegram Bot 管理脚本"
    echo "======================================${NC}"
    echo ""
    echo "请选择操作:"
    echo ""
    echo "  1. 检查环境依赖"
    echo "  2. 检查后台服务状态"
    echo "  3. 安装/更新后台服务"
    echo "  4. 控制台启动 (调试模式)"
    echo "  5. 卸载后台服务"
    echo "  6. 查看实时日志"
    echo "  0. 退出"
    echo ""
}

# 主循环
main() {
    while true; do
        show_menu
        read -p "请输入选项 [0-6]: " choice
        echo ""
        
        case $choice in
            1)
                check_dependencies
                read -p "按回车键继续..." 
                ;;
            2)
                check_service
                read -p "按回车键继续..." 
                ;;
            3)
                install_service
                read -p "按回车键继续..." 
                ;;
            4)
                console_start
                read -p "按回车键继续..." 
                ;;
            5)
                uninstall_service
                read -p "按回车键继续..." 
                ;;
            6)
                if command -v systemctl &> /dev/null; then
                    echo -e "${YELLOW}实时日志 (按 Ctrl+C 退出):${NC}"
                    echo ""
                    journalctl -u "${SERVICE_NAME}" -f
                else
                    echo -e "${RED}systemd 不可用${NC}"
                    read -p "按回车键继续..." 
                fi
                ;;
            0)
                echo -e "${GREEN}再见!${NC}"
                exit 0
                ;;
            *)
                echo -e "${RED}无效选项${NC}"
                read -p "按回车键继续..." 
                ;;
        esac
    done
}

# 如果带参数运行,直接执行对应功能
if [ $# -gt 0 ]; then
    case $1 in
        check|1)
            check_dependencies
            ;;
        status|2)
            check_service
            ;;
        install|3)
            install_service
            ;;
        start|4)
            console_start
            ;;
        uninstall|5)
            uninstall_service
            ;;
        *)
            echo "用法: $0 [check|status|install|start|uninstall]"
            exit 1
            ;;
    esac
else
    # 无参数时显示菜单
    main
fi
