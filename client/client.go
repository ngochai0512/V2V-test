package main

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/alecthomas/kong"
	"github.com/gorilla/websocket"
)

var Version = "dev"

var CLI struct {
	Version   kong.VersionFlag `help:"Hiển thị phiên bản (Git Commit Hash)" short:"v"`
	Server    string           `help:"Link server WebSocket" required:"" short:"s"`
	Username  string           `help:"Tên người dùng của bạn" default:"Anonymous" short:"u"`
	UserAgent string           `help:"Tùy chỉnh User-Agent" default:"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36" short:"a"`
	Info      bool             `help:"Kiểm tra thông tin trạng thái của Server" short:"i"`
}

func normalizeURL(input string) string {
	input = strings.TrimSpace(input)

	if !strings.HasPrefix(input, "http://") && !strings.HasPrefix(input, "https://") &&
		!strings.HasPrefix(input, "ws://") && !strings.HasPrefix(input, "wss://") {
		input = "wss://" + input
	}

	input = strings.Replace(input, "http://", "ws://", 1)
	input = strings.Replace(input, "https://", "wss://", 1)

	u, err := url.Parse(input)
	if err == nil {
		if u.Path == "" || u.Path == "/" {
			u.Path = "/ws"
		}
		return u.String()
	}

	return input
}

func extractSender(msg string) string {
	if strings.HasPrefix(msg, "[") {
		if idx := strings.Index(msg, "]:"); idx != -1 {
			return msg[1:idx]
		}
	}
	return ""
}

func checkServerInfo(input string) {
	input = strings.TrimSpace(input)

	if strings.HasPrefix(input, "ws://") {
		input = strings.Replace(input, "ws://", "http://", 1)
	} else if strings.HasPrefix(input, "wss://") {
		input = strings.Replace(input, "wss://", "https://", 1)
	} else if !strings.HasPrefix(input, "http://") && !strings.HasPrefix(input, "https://") {
		if strings.Contains(input, "localhost") || strings.HasPrefix(input, "127.") {
			input = "http://" + input
		} else {
			input = "https://" + input
		}
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(input)
	if err != nil {
		fmt.Println("❌ Lỗi khi lấy thông tin:", err)
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		fmt.Println("❌ Lỗi khi đọc dữ liệu:", err)
		return
	}

	fmt.Println("\n" + string(body))
}

func main() {
	kong.Parse(&CLI, kong.Vars{
		"version": Version,
	})

	if CLI.Info {
		checkServerInfo(CLI.Server)
		return
	}

	wsURL := normalizeURL(CLI.Server)
	username := strings.TrimSpace(CLI.Username)

	headers := http.Header{}
	headers.Add("User-Agent", CLI.UserAgent)

	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		fmt.Println("❌ Không thể kết nối:", err)
		if resp != nil {
			fmt.Printf("👉 HTTP Status Code: %d\n", resp.StatusCode)

			if resp.StatusCode == 200 {
				bodyBytes, _ := io.ReadAll(resp.Body)
				fmt.Printf("📦 Nội dung phản hồi: %s\n", string(bodyBytes))
			}
		}
		return
	}
	defer conn.Close()

	conn.WriteMessage(websocket.TextMessage, []byte(username))

	fmt.Println("Đã kết nối với username:", username)
	fmt.Println("Gõ tin nhắn để chat, /help để hiện trợ giúp\n")
	fmt.Print("| > ")

	quitting := make(chan bool, 1)
	var hideJoinLeave bool
	var hideMu sync.RWMutex

	go func() {
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				select {
				case <-quitting:
					return
				default:
					fmt.Print("\r\033[K")
					fmt.Println("\n ❌ Mất kết nối server")
					os.Exit(1)
				}
			}

			text := string(msg)

			hideMu.RLock()
			isHiding := hideJoinLeave
			hideMu.RUnlock()

			if isHiding && strings.Contains(text, "[Hệ thống]:") && (strings.Contains(text, "đã tham gia") || strings.Contains(text, "đã rời")) {
				continue
			}

			paddedText := "| " + strings.ReplaceAll(text, "\n", "\n  ")

			fmt.Print("\r\033[K")
			fmt.Println(paddedText)
			fmt.Print("| > ")
		}
	}()

	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		text := scanner.Text()

		if text == "/quit" || text == "/q" {
			quitting <- true
			conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))

			fmt.Print("\r\033[K")
			fmt.Println("👋 Đang ngắt kết nối... Tạm biệt!")

			time.Sleep(500 * time.Millisecond)
			break
		}

		if text == "/help" || text == "/h" {
			fmt.Print("\r\033[K")
			fmt.Println("  [Trợ giúp]: Danh sách các lệnh có thể sử dụng:")
			fmt.Println("   - /help, /h      : Hiển thị bảng trợ giúp này")
			fmt.Println("   - /quit, /q      : Rời phòng chat và tắt ứng dụng")
			fmt.Println("   - /hideJoin, /hj : Bật/tắt chế độ ẩn thông báo người khác ra vào phòng")
			fmt.Print("| > ")
			continue
		}

		if text == "/hideJoin" || text == "/hj" {
			hideMu.Lock()
			hideJoinLeave = !hideJoinLeave
			status := "ĐÃ ẨN"
			if !hideJoinLeave {
				status = "ĐÃ HIỆN"
			}
			hideMu.Unlock()

			fmt.Print("\r\033[K")
			fmt.Printf("| [Local]: %s thông báo người dùng ra/vào phòng.\n", status)
			fmt.Print("| > ")
			continue
		}

		if strings.TrimSpace(text) != "" {
			text = strings.ReplaceAll(text, "\\n", "\n")

			fmt.Print("\033[1A\r\033[K")

			paddedText := "| Bạn: " + strings.ReplaceAll(text, "\n", "\n|      ")
			fmt.Println(paddedText)

			fmt.Print("| > ")

			err := conn.WriteMessage(websocket.TextMessage, []byte(text))
			if err != nil {
				fmt.Println("❌ Lỗi gửi tin nhắn:", err)
				break
			}
		} else {
			fmt.Print("\r\033[K| > ")
		}
	}
}
