package zsy_webview

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
)

func FastRandPort9000() int {
	var b [2]byte
	_, _ = rand.Read(b[:])
	val := binary.BigEndian.Uint16(b[:])
	return int(val%(65535-9000+1)) + 9000
}

func IsPortUsed(port int) bool {
	portStr := ":" + strconv.Itoa(port)
	var cmd *exec.Cmd

	if runtime.GOOS == "windows" {
		// Windows 下执行 netstat -ano | findstr :端口
		cmd = exec.Command("cmd", "/c", fmt.Sprintf("netstat -ano | findstr %s", portStr))
	} else {
		// Linux/macOS 下执行 netstat -an | grep :端口
		cmd = exec.Command("sh", "-c", fmt.Sprintf("netstat -an | grep %s", portStr))
	}

	var out bytes.Buffer
	cmd.Stdout = &out
	_ = cmd.Run() // 如果没有找到匹配项，Run 会返回 error

	// 如果输出包含内容，说明端口已经被占用
	return out.Len() > 0
}
