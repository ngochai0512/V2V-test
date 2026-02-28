package main

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"
	"unicode"
	"unicode/utf8"

	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
)

type AppConfig struct {
	MaxConnectionsPerIP int
	MaxMessageLength    int
	MaxMessageLine      int
	MessageCooldown     time.Duration
	MaxHistoryBytes     int
	MaxHistorySend      int
	MaxUsernameLength   int
	ConnectionCooldown  time.Duration
	Port                string
	StatusURL           string
	DownloadURL         string
	HomepageURL         string
	InstanceID          string
	Timezone            *time.Location
}

var cfg AppConfig

var (
	serverStartTime = time.Now()

	clients   = make(map[*websocket.Conn]string)
	clientsMu sync.Mutex

	ipCounts   = make(map[string]int)
	ipCountsMu sync.Mutex

	lastConnectTime = make(map[string]time.Time)
	lastConnectMu   sync.Mutex

	chatHistory     []string
	chatHistorySize int
	historyMu       sync.RWMutex

	lastMessageDate   string
	lastMessageDateMu sync.Mutex

	upgrader = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}

	ansiRegex = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)
)

func getEnvAsLocationOptional(key string, fallback string) *time.Location {
	val, exists := os.LookupEnv(key)
	if !exists || val == "" {
		val = fallback
	}
	loc, err := time.LoadLocation(val)
	if err != nil {
		log.Printf("⚠️ Cảnh báo: Múi giờ '%s' không hợp lệ. Đang dùng mặc định (Local).", val)
		return time.Local
	}
	return loc
}

func getSmartEnv(key string) string {
	val, exists := os.LookupEnv(key)
	if !exists || val == "" {
		log.Fatalf("❌ CRITICAL ERROR: Thiếu biến môi trường bắt buộc: %s", key)
	}

	sysVal := os.Getenv(val)
	if sysVal != "" {
		return sysVal
	}
	return val
}

func getEnvAsInt(key string) int {
	val, exists := os.LookupEnv(key)
	if !exists || val == "" {
		log.Fatalf("❌ CRITICAL ERROR: Thiếu biến môi trường bắt buộc: %s", key)
	}
	parsed, err := strconv.Atoi(val)
	if err != nil {
		log.Fatalf("❌ Lỗi định dạng số ở biến %s: %v", key, err)
	}
	return parsed
}

func getEnvAsDuration(key string) time.Duration {
	val, exists := os.LookupEnv(key)
	if !exists || val == "" {
		log.Fatalf("❌ CRITICAL ERROR: Thiếu biến môi trường bắt buộc: %s", key)
	}
	parsed, err := time.ParseDuration(val)
	if err != nil {
		log.Fatalf("❌ Lỗi định dạng thời gian ở biến %s (ví dụ đúng: 200ms, 5s): %v", key, err)
	}
	return parsed
}

func lastAfterDash(s string) string {
	if i := strings.LastIndex(s, "-"); i != -1 {
		return s[i+1:]
	}
	return s
}

func addMessageToHistory(msg string) {
	historyMu.Lock()
	defer historyMu.Unlock()

	msgSize := len(msg)
	chatHistory = append(chatHistory, msg)
	chatHistorySize += msgSize

	for chatHistorySize > cfg.MaxHistoryBytes && len(chatHistory) > 0 {
		oldestSize := len(chatHistory[0])
		chatHistorySize -= oldestSize

		chatHistory[0] = ""
		chatHistory = chatHistory[1:]
	}
}

func broadcast(message string, sender *websocket.Conn) {
	addMessageToHistory(message)

	clientsMu.Lock()
	defer clientsMu.Unlock()

	for conn := range clients {
		if conn != sender {
			if err := conn.WriteMessage(websocket.TextMessage, []byte(message)); err != nil {
				conn.Close()
				delete(clients, conn)
			}
		}
	}
}

func checkAndBroadcastDate(now time.Time) {
	currentDate := now.Format("02/01/2006")

	lastMessageDateMu.Lock()
	defer lastMessageDateMu.Unlock()

	if lastMessageDate == "" || lastMessageDate != currentDate {
		lastMessageDate = currentDate

		dateMsg := fmt.Sprintf("\x1b[36m--- Ngày %s ---\x1b[0m", currentDate)

		broadcast(dateMsg, nil)
	}
}

// To prevent IP spoofing, only accept IPs sent from Cloudflare
// Change this getClientIP function if you are not using Cloudflare
func getClientIP(r *http.Request) string {
	var ip string

	if cfIP := r.Header.Get("CF-Connecting-IP"); cfIP != "" {
		ip = cfIP
	} else {
		ip, _, _ = net.SplitHostPort(r.RemoteAddr)
	}

	parsedIP := net.ParseIP(strings.TrimSpace(ip))
	if parsedIP == nil {
		fallbackIP, _, _ := net.SplitHostPort(r.RemoteAddr)
		return fallbackIP
	}

	return parsedIP.String()
}

func sanitizeString(text string) string {
	text = ansiRegex.ReplaceAllString(text, "")
	return strings.Map(func(r rune) rune {
		if r == '\n' || unicode.IsGraphic(r) {
			if !unicode.Is(unicode.Mn, r) && !unicode.Is(unicode.Me, r) {
				return r
			}
		}
		return -1
	}, text)
}

func chatHandler(w http.ResponseWriter, r *http.Request) {
	clientIP := getClientIP(r)

	lastConnectMu.Lock()
	if lastTime, exists := lastConnectTime[clientIP]; exists {
		if time.Since(lastTime) < cfg.ConnectionCooldown {
			lastConnectMu.Unlock()
			log.Printf("⛔ Từ chối: %s kết nối ra/vào quá nhanh.\n", clientIP)
			http.Error(w, "Bạn thao tác ra/vào quá nhanh! Vui lòng đợi vài giây rồi thử lại.", http.StatusTooManyRequests)
			return
		}
	}
	lastConnectTime[clientIP] = time.Now()
	lastConnectMu.Unlock()

	ipCountsMu.Lock()
	if ipCounts[clientIP] >= cfg.MaxConnectionsPerIP {
		ipCountsMu.Unlock()
		log.Printf("⛔ Từ chối: %s đã vượt quá giới hạn %d kết nối.\n", clientIP, cfg.MaxConnectionsPerIP)
		http.Error(w, "Bạn đã mở quá nhiều kết nối từ địa chỉ IP này.", http.StatusTooManyRequests)
		return
	}
	ipCounts[clientIP]++
	ipCountsMu.Unlock()

	defer func() {
		ipCountsMu.Lock()
		ipCounts[clientIP]--
		if ipCounts[clientIP] <= 0 {
			delete(ipCounts, clientIP)
		}
		ipCountsMu.Unlock()
	}()

	log.Printf("🔌 New request | Client IP: %s | Proxy IP: %s | Upgrade: %s\n", clientIP, r.RemoteAddr, r.Header.Get("Upgrade"))

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("❌ Upgrade error:", err)
		return
	}
	defer conn.Close()

	_, nameMsg, err := conn.ReadMessage()
	if err != nil {
		return
	}
	username := sanitizeString(string(nameMsg))
	if utf8.RuneCountInString(username) > cfg.MaxUsernameLength {
		runes := []rune(username)
		username = string(runes[:cfg.MaxUsernameLength])
	}
	if username == "" {
		username = "Anonymous"
	}

	hash := md5.Sum([]byte(clientIP))
	ipSuffix := hex.EncodeToString(hash[:])[:4]
	displayName := fmt.Sprintf("%s#%s", username, ipSuffix)

	clientsMu.Lock()
	clients[conn] = displayName
	clientsMu.Unlock()

	historyMu.RLock()
	historyLen := len(chatHistory)

	if historyLen > 0 {
		startIndex := 0
		if historyLen > cfg.MaxHistorySend {
			startIndex = historyLen - cfg.MaxHistorySend
		}

		conn.WriteMessage(websocket.TextMessage, []byte("--- Lịch sử chat gần đây ---"))

		for i := startIndex; i < historyLen; i++ {
			time.Sleep(5 * time.Millisecond)

			conn.WriteMessage(websocket.TextMessage, []byte(chatHistory[i]))
		}

		conn.WriteMessage(websocket.TextMessage, []byte("--- Kết thúc lịch sử ---"))
	}
	historyMu.RUnlock()

	joinTime := time.Now().In(cfg.Timezone)
	checkAndBroadcastDate(joinTime)
	joinTimeStr := joinTime.Format("15:04")

	joinMsg := fmt.Sprintf("\x1b[90m%s\x1b[0m [Hệ thống]: %s đã tham gia phòng chat!", joinTimeStr, displayName)
	log.Printf("🟢 [JOIN] %s (IP: %s)\n", displayName, clientIP)
	broadcast(joinMsg, conn)

	lastMessageTime := time.Time{}

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			break
		}

		text := string(msg)
		text = sanitizeString(text)

		if utf8.RuneCountInString(text) > cfg.MaxMessageLength {
			warning := fmt.Sprintf("[Hệ thống]: Tin nhắn của bạn quá dài (tối đa %d ký tự). Hãy chia nhỏ ra nhé!", cfg.MaxMessageLength)
			conn.WriteMessage(websocket.TextMessage, []byte(warning))
			continue
		}

		if strings.Count(text, "\n") > cfg.MaxMessageLine {
			warning := "[Hệ thống]: Tin nhắn chứa quá nhiều dòng. Vui lòng gộp lại để tránh làm trôi khung chat!"
			conn.WriteMessage(websocket.TextMessage, []byte(warning))
			continue
		}

		if time.Since(lastMessageTime) < cfg.MessageCooldown {
			warning := fmt.Sprintf("[Hệ thống]: Bạn đang chat quá nhanh! Vui lòng đợi %v giữa các tin nhắn.", cfg.MessageCooldown)
			conn.WriteMessage(websocket.TextMessage, []byte(warning))
			continue
		}

		lastMessageTime = time.Now()

		now := time.Now().In(cfg.Timezone)
		checkAndBroadcastDate(now)
		timeStr := now.Format("15:04")

		chatMsg := fmt.Sprintf("\x1b[90m%s\x1b[0m %s: %s", timeStr, displayName, text)
		log.Printf("💬 [MSG từ %s]: %s\n", clientIP, chatMsg)
		broadcast(chatMsg, conn)
	}

	clientsMu.Lock()
	delete(clients, conn)
	clientsMu.Unlock()

	leaveTime := time.Now().In(cfg.Timezone)
	checkAndBroadcastDate(leaveTime)
	leaveTimeStr := leaveTime.Format("15:04")

	leaveMsg := fmt.Sprintf("\x1b[90m%s\x1b[0m [Hệ thống]: %s đã rời phòng chat.", leaveTimeStr, displayName)
	log.Printf("🔴 [LEAVE] %s (IP: %s)\n", displayName, clientIP)
	broadcast(leaveMsg, nil)
}

func main() {
	err := godotenv.Load()
	if err != nil {
		_ = godotenv.Load("/etc/secrets/.env")
	}

	cfg = AppConfig{
		MaxConnectionsPerIP: getEnvAsInt("MAX_CONNECTIONS_PER_IP"),
		MaxMessageLength:    getEnvAsInt("MAX_MESSAGE_LENGTH"),
		MaxMessageLine:      getEnvAsInt("MAX_MESSAGE_LINE"),
		MessageCooldown:     getEnvAsDuration("MESSAGE_COOLDOWN"),
		MaxHistoryBytes:     getEnvAsInt("MAX_HISTORY_BYTES"),
		MaxHistorySend:      getEnvAsInt("MAX_HISTORY_SEND"),
		MaxUsernameLength:   getEnvAsInt("MAX_USERNAME_LENGTH"),
		ConnectionCooldown:  getEnvAsDuration("CONNECTION_COOLDOWN"),
		Port:                getSmartEnv("PORT"),
		StatusURL:           getSmartEnv("STATUS_URL"),
		DownloadURL:         getSmartEnv("DOWNLOAD_URL"),
		HomepageURL:         getSmartEnv("HOMEPAGE_URL"),
		InstanceID:          lastAfterDash(getSmartEnv("INSTANCE_ID")),
		Timezone:            getEnvAsLocationOptional("TIMEZONE", "Asia/Ho_Chi_Minh"),
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.ToLower(r.Header.Get("Upgrade")) == "websocket" {
			chatHandler(w, r)
			return
		}

		uptime := time.Since(serverStartTime).Round(time.Second)

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "WebSocket server is running...\n")
		fmt.Fprintln(w, "Mô tả      : Hệ thống chat ẩn danh")
		fmt.Fprintln(w, "Giao thức  : WebSocket")
		fmt.Fprintf(w, "Instance ID: %s\n", cfg.InstanceID)
		fmt.Fprintf(w, "Uptime     : %s\n", uptime.String())
		fmt.Fprintf(w, "Múi giờ    : %s\n", cfg.Timezone)
		fmt.Fprintf(w, "Trạng thái : %s\n", cfg.StatusURL)
		fmt.Fprintln(w, "------------------------------------")
		fmt.Fprintf(w, "Tải Client : %s\n", cfg.DownloadURL)
		fmt.Fprintf(w, "Homepage   : %s\n", cfg.HomepageURL)
	})

	server := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Println("🚀 Server đang chạy tại port", cfg.Port)
	log.Fatal(server.ListenAndServe())
}
