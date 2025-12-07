#!/usr/bin/env bash
set -euo pipefail  # 严格模式: 遇到错误退出,未定义变量报错,管道错误检测

# 获取脚本所在目录的绝对路径
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# 调试输出
echo "[DEBUG] SCRIPT_DIR=$SCRIPT_DIR" >&2

export PATH=~/bin:/usr/local/bin:/usr/local/sbin:/usr/bin:/usr/sbin:/sbin:/bin
tdl_data_dir="${SCRIPT_DIR}/.tdl"  # TDL数据目录（登录信息、二进制等）
tdl_dir="${tdl_data_dir}"
tdl_bin="${tdl_dir}/tdl"
version_file="${tdl_dir}/.version"
lock_dir="/tmp/tdl_locks"  # 锁文件目录

# 创建数据目录
mkdir -p "${tdl_data_dir}/data"

# 颜色定义
Error="\033[31m[错误]\033[0m"
Info="\033[32m[信息]\033[0m"
Warning="\033[33m[警告]\033[0m"

# 创建锁目录
mkdir -p "$lock_dir"

#检查系统版本
check_sys() {
    if [[ -f /etc/redhat-release ]]; then
        release="centos"
    elif grep -q -E -i "debian" /etc/issue 2>/dev/null; then
        release="debian"
    elif grep -q -E -i "ubuntu" /etc/issue 2>/dev/null; then
        release="ubuntu"
    elif grep -q -E -i "centos|red hat|redhat" /etc/issue 2>/dev/null; then
        release="centos"
    elif grep -q -E -i "debian" /proc/version 2>/dev/null; then
        release="debian"
    elif grep -q -E -i "ubuntu" /proc/version 2>/dev/null; then
        release="ubuntu"
    elif grep -q -E -i "centos|red hat|redhat" /proc/version 2>/dev/null; then
        release="centos"
    fi
    ARCH=$(uname -m)
    if command -v dpkg &>/dev/null; then
        dpkgARCH=$(dpkg --print-architecture | awk -F- '{ print $NF }')
    fi
}

#获取当前版本
get_current_ver() {
    if [[ -f "${version_file}" ]]; then
        cat "${version_file}"
    else
        echo "unknown"
    fi
}

#获取最新版本
get_latest_ver() {
    local latest_ver
    latest_ver=$(
        {
            wget -t2 -T3 -qO- "https://api.github.com/repos/iyear/tdl/releases/latest" ||
                wget -t2 -T3 -qO- "https://gh-api.p3terx.com/repos/iyear/tdl/releases/latest"
        } | grep -o '"tag_name": ".*"' | head -n 1 | cut -d'"' -f4
    )
    
    if [[ -z "${latest_ver}" ]]; then
        echo -e "${Warning} 无法获取最新版本信息" >&2
        return 1
    fi
    
    echo "${latest_ver}"
}

#检查并更新版本
check_and_update() {
    local current_ver latest_ver
    
    # 调试输出
    echo -e "${Info} check_and_update 函数开始" 
    echo -e "${Info} SCRIPT_DIR=$SCRIPT_DIR" 
    echo -e "${Info} tdl_bin=$tdl_bin" 
    echo -e "${Info} version_file=$version_file" 
    
    # 检查二进制文件是否存在
    if [[ ! -f "${tdl_bin}" ]]; then
        echo -e "${Warning} TDL 二进制文件不存在，开始下载..."
        install_tdl_binary
        return $?
    fi
    
    # 获取当前版本
    current_ver=$(get_current_ver)
    echo -e "${Info} 当前版本: ${current_ver}"
    
    # 获取最新版本
    latest_ver=$(get_latest_ver)
    if [[ $? -ne 0 || -z "${latest_ver}" ]]; then
        echo -e "${Warning} 跳过版本检查，使用现有版本"
        return 0
    fi
    
    echo -e "${Info} 最新版本: ${latest_ver}"
    
    # 比较版本
    if [[ "${current_ver}" != "${latest_ver}" ]]; then
        echo -e "${Info} 发现新版本，开始更新..."
        install_tdl_binary "${latest_ver}"
        return $?
    else
        echo -e "${Info} 已是最新版本"
        return 0
    fi
}
#下载并安装二进制文件
install_tdl_binary() {
    local target_ver="${1:-}"
    
    # 如果未指定版本，获取最新版本
    if [[ -z "${target_ver}" ]]; then
        target_ver=$(get_latest_ver)
        if [[ $? -ne 0 || -z "${target_ver}" ]]; then
            echo -e "${Error} 无法获取版本信息"
            return 1
        fi
    fi
    
    # 检查系统架构
    check_sys
    
    if [[ $ARCH == i*86 || $dpkgARCH == i*86 ]]; then
        ARCH="32bit"
    elif [[ $ARCH == "x86_64" || $dpkgARCH == "amd64" ]]; then
        ARCH="64bit"
    elif [[ $ARCH == "aarch64" || $dpkgARCH == "arm64" ]]; then
        ARCH="arm64"
    elif [[ $ARCH == "armv7l" || $dpkgARCH == "armhf" ]]; then
        ARCH="armhf"
    else
        echo -e "${Error} 不支持此 CPU 架构: ${ARCH}"
        return 1
    fi
    
    # 创建目录
    mkdir -p "${tdl_dir}"
    cd "${tdl_dir}" || return 1
    
    # 下载
    local DOWNLOAD_URL="https://github.com/iyear/tdl/releases/download/${target_ver}/tdl_Linux_${ARCH}.tar.gz"
    echo -e "${Info} 下载版本: ${target_ver} (${ARCH})"
    
    {
        wget -t2 -T10 -O- "${DOWNLOAD_URL}" ||
            wget -t2 -T10 -O- "https://gh-acc.p3terx.com/${DOWNLOAD_URL}"
    } | tar -zx
    
    if [[ ! -f "${tdl_bin}" ]]; then
        echo -e "${Error} 二进制文件下载失败"
        return 1
    fi
    
    # 设置权限
    chmod +x "${tdl_bin}"
    
    # 保存版本信息
    echo "${target_ver}" > "${version_file}"
    
    echo -e "${Info} TDL ${target_ver} 安装成功"
    return 0
}
#登录处理
login_tdl() {
    local namespace="${1:-default}"
    
    # 只在 Bot 端显示提示
    echo "[STATUS]🔐 需要登录"
    echo "[STATUS]📺 请到服务器控制台查看二维码并使用 Telegram 扫描登录"
    
    # 在控制台执行登录命令，显示二维码，允许交互式输入（如2FA密码）
    # 检测是否有 tty 可用
    if [ -t 0 ]; then
        # 标准输入是终端，直接运行
        "${tdl_bin}" login -T qr -n "$namespace" --storage "type=bolt,path=${tdl_data_dir}/data"
    elif [ -c /dev/tty ]; then
        # 尝试使用 /dev/tty
        "${tdl_bin}" login -T qr -n "$namespace" --storage "type=bolt,path=${tdl_data_dir}/data" < /dev/tty
    else
        # 没有可用的 tty，直接运行（可能无法交互）
        "${tdl_bin}" login -T qr -n "$namespace" --storage "type=bolt,path=${tdl_data_dir}/data"
    fi
    
    local login_result=$?
    
    if [ $login_result -eq 0 ]; then
        echo "[STATUS]✅ 登录成功"
    else
        echo "[STATUS]❌ 登录失败 (退出码: ${login_result})"
    fi
    
    return $login_result
}

#执行转发
run_tdl() {
    local str="${1:-}"
    local task_id="${2:-1}"  # 任务ID用于锁文件命名
    local lock_file="${lock_dir}/task_${task_id}.lock"
    local lock_fd
    
    if test -z "$str"; then
        echo "请输入需下载TG文件的链接，多个连接使用空格分隔"
        read -r str
    fi
    
    # 获取独占锁,每个任务有独立的锁文件
    exec {lock_fd}>"$lock_file"
    if ! flock -n "$lock_fd"; then
        echo "[STATUS]⏳ 正在获取资源锁..."
        flock "$lock_fd"  # 阻塞等待锁
    fi
    
    # 转发开始
    echo -e "[STATUS]📡 开始转发任务"
    
    # 使用临时文件保存输出
    local temp_output="/tmp/tdl_forward_${task_id}_$$.txt"
    touch "$temp_output" || true
    
    # 在后台执行转发，保存 PID (即使失败也继续)
    "${tdl_bin}" forward --from "$str" --to 1838605845 --single -n "default" --mode clone --storage "type=bolt,path=${tdl_data_dir}/data" > "$temp_output" 2>&1 &
    local forward_pid=$!
    
    # 等待一下确保文件有内容
    sleep 0.2
    
    # 实时监控并解析输出
    tail -f "$temp_output" 2>/dev/null | while IFS= read -r line; do
        # 清理 ANSI 转义序列和空白
        clean_line=$(echo "$line" | tr -d '\r' | sed 's/\x1b\[[0-9;]*[A-Za-z]//g; s/\x1b\[[0-9;]*m//g; s/^[[:space:]]*//; s/[[:space:]]*$//')
        
        # 过滤掉系统监控信息
        if echo "$clean_line" | grep -qiE '(CPU:|Memory:|Goroutines:)'; then
            continue
        fi
        
        # 原始输出到控制台
        echo "$line"
        
        # 只匹配包含进度条或速度信息的行(通常包含 / 和单位)
        # 例如: "123.45 MB/s" 或 "50.00% | 100 MB / 200 MB"
        if echo "$clean_line" | grep -qE '([0-9]+\.[0-9]+%|[0-9]+%).*(/|MB|KB|GB)'; then
            percentage=$(echo "$clean_line" | grep -oE '[0-9]+\.?[0-9]*%' | head -1 | tr -d '%')
            if [ -n "$percentage" ]; then
                # 检测下载还是上传
                if echo "$clean_line" | grep -qiE '(download|↓|⬇)'; then
                    echo "[STATUS]⬇️ 下载进度: ${percentage}%"
                elif echo "$clean_line" | grep -qiE '(upload|↑|⬆)'; then
                    echo "[STATUS]⬆️ 上传进度: ${percentage}%"
                else
                    echo "[STATUS]⏳ 转发进度: ${percentage}%"
                fi
            fi
        fi
        
        # 检测完成
        if echo "$clean_line" | grep -qiE '(success|complete|done|finished)'; then
            echo "[STATUS]✅ 转发成功"
        fi
    done &
    local tail_pid=$!
    
    # 等待进程结束或检测到登录错误
    local need_login=false
    local exit_code=0
    
    while kill -0 "$forward_pid" 2>/dev/null || true; do
        # 检查进程是否还在运行
        if ! kill -0 "$forward_pid" 2>/dev/null; then
            break
        fi
        
        if [ -f "$temp_output" ]; then
            # 检测多种登录错误
            if grep -qi "not authorized" "$temp_output" 2>/dev/null || grep -qi "unauthorized" "$temp_output" 2>/dev/null || grep -qi "please login first" "$temp_output" 2>/dev/null; then
                need_login=true
                # 杀死转发进程
                kill "$forward_pid" 2>/dev/null || true
                wait "$forward_pid" 2>/dev/null || true
                break
            fi
        fi
        sleep 0.5
    done
    
    # 等待一下让 tail 进程处理完最后的输出
    sleep 1
    
    # 停止 tail (杀死整个进程组)
    pkill -P $tail_pid 2>/dev/null || true
    kill $tail_pid 2>/dev/null || true
    wait $tail_pid 2>/dev/null || true
    
    # 获取退出码
    wait "$forward_pid" 2>/dev/null || true
    exit_code=$?
    
    # 输出最终状态
    if [ $exit_code -eq 0 ]; then
        echo "[STATUS]✅ 转发完成"
    else
        echo "[STATUS]❌ 转发失败 (退出码: ${exit_code})"
    fi
    
    # 如果还未检测到登录错误，检查一次输出文件（不依赖退出码）
    if [ "$need_login" = false ] && [ -f "$temp_output" ]; then
        # 检查整个文件内容,不区分大小写
        if grep -qi "not authorized" "$temp_output" 2>/dev/null; then
            need_login=true
        elif grep -qi "unauthorized" "$temp_output" 2>/dev/null; then
            need_login=true
        elif grep -qi "please login first" "$temp_output" 2>/dev/null; then
            need_login=true
        fi
    fi
    
    # 检查是否需要登录
    if [ "$need_login" = true ]; then
        # 释放锁
        flock -u "$lock_fd"
        
        # 执行登录（交互式）
        login_tdl "default"
        login_result=$?
        
        # 清理临时文件
        rm -f "$temp_output"
        
        # 重新获取锁
        flock "$lock_fd"
        
        if [ $login_result -eq 0 ]; then
            # 登录成功，重新执行转发
            echo -e "[STATUS]🔄 重新开始转发任务"
            exec "$0" "$str" "$task_id"
        else
            # 登录失败
            flock -u "$lock_fd"
            exec {lock_fd}>&-
            rm -f "$lock_file"
            return 1
        fi
    fi
    
    # 清理临时文件
    rm -f "$temp_output"
    
    # 释放锁
    flock -u "$lock_fd"
    exec {lock_fd}>&-
    rm -f "$lock_file"
    
    return $exit_code
}

#主函数
main() {
    local param="${1:-}"
    local task_id="${2:-1}"
    
    # 检查并更新版本
    check_and_update
    
    # 执行命令 (传递task_id用于锁管理)
    run_tdl "$param" "$task_id"
}

# 执行主函数
main "${1:-}" "${2:-1}"
