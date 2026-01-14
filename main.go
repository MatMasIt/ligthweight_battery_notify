package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
	"gopkg.in/yaml.v3"
)

type Config struct {
	AppName         string       `yaml:"app_name"`
	PollInterval    int          `yaml:"poll_interval"` // seconds
	LowBattery      BatteryLevel `yaml:"low_battery"`
	CriticalBattery BatteryLevel `yaml:"critical_battery"`
}

type BatteryLevel struct {
	Threshold int    `yaml:"threshold"`
	Title     string `yaml:"title"`
	Icon      string `yaml:"icon"`
	Sound     string `yaml:"sound"` // Optional
	Message   string `yaml:"message"`
}

type Battery interface {
	Capacity() (int, error)
	IsCharging() (bool, error)
}

type SysfsBattery struct {
	capacityPath string
	statusPath   string
}

type Notifier interface {
	Send(summary, body, urgency, icon string) (uint32, error)
	Close(id uint32) error
}

type DBusNotifier struct {
	conn    *dbus.Conn
	obj     dbus.BusObject
	appName string
}

type Monitor struct {
	config         Config
	battery        Battery
	notifier       Notifier
	currentLevel   string // "normal", "low", "critical"
	notificationID uint32
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, err
	}

	// Set default poll interval
	if config.PollInterval == 0 {
		config.PollInterval = 5
	}

	// Set default app name
	if config.AppName == "" {
		config.AppName = "Battery Monitor"
	}

	return &config, nil
}

// SysfsBattery implementation
func NewSysfsBattery() (*SysfsBattery, error) {
	batteries := []string{
		"/sys/class/power_supply/BAT0",
		"/sys/class/power_supply/BAT1",
		"/sys/class/power_supply/battery",
	}

	for _, path := range batteries {
		capacityPath := filepath.Join(path, "capacity")
		statusPath := filepath.Join(path, "status")

		if _, err := os.Stat(capacityPath); err == nil {
			if _, err := os.Stat(statusPath); err == nil {
				log.Printf("Found battery at: %s", path)
				return &SysfsBattery{
					capacityPath: capacityPath,
					statusPath:   statusPath,
				}, nil
			}
		}
	}

	return nil, fmt.Errorf("no battery found")
}

func (b *SysfsBattery) Capacity() (int, error) {
	data, err := os.ReadFile(b.capacityPath)
	if err != nil {
		return 0, err
	}
	capacity, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, err
	}
	return capacity, nil
}

func (b *SysfsBattery) IsCharging() (bool, error) {
	data, err := os.ReadFile(b.statusPath)
	if err != nil {
		return false, err
	}
	status := strings.TrimSpace(string(data))
	return status == "Charging" || status == "Full", nil
}

// DBusNotifier implementation
func NewDBusNotifier(appName string) (*DBusNotifier, error) {
	conn, err := dbus.SessionBus()
	if err != nil {
		return nil, err
	}

	obj := conn.Object("org.freedesktop.Notifications", "/org/freedesktop/Notifications")

	return &DBusNotifier{
		conn:    conn,
		obj:     obj,
		appName: appName,
	}, nil
}

func (n *DBusNotifier) Send(summary, body, urgency, icon string) (uint32, error) {
	var urgencyLevel byte
	switch urgency {
	case "critical":
		urgencyLevel = 2
	case "normal":
		urgencyLevel = 1
	default:
		urgencyLevel = 0
	}

	hints := map[string]dbus.Variant{
		"urgency":   dbus.MakeVariant(urgencyLevel),
		"resident":  dbus.MakeVariant(true),  // Keep in notification center
		"transient": dbus.MakeVariant(false), // Don't auto-dismiss
	}

	call := n.obj.Call("org.freedesktop.Notifications.Notify", 0,
		n.appName,  // app_name
		uint32(0),  // replaces_id (we'll manage this in Monitor)
		icon,       // app_icon
		summary,    // summary
		body,       // body
		[]string{}, // actions
		hints,      // hints
		int32(0),   // expire_timeout (0 = no auto-dismiss)
	)

	if call.Err != nil {
		return 0, call.Err
	}

	var id uint32
	err := call.Store(&id)
	return id, err
}

func (n *DBusNotifier) Close(id uint32) error {
	if id == 0 {
		return nil
	}
	call := n.obj.Call("org.freedesktop.Notifications.CloseNotification", 0, id)
	return call.Err
}

// Monitor implementation
func (m *Monitor) playSound(soundFile string) {
	if soundFile == "" {
		return
	}

	// Expand home directory
	if strings.HasPrefix(soundFile, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			soundFile = filepath.Join(home, soundFile[2:])
		}
	}

	// Try paplay first, then aplay
	go func() {
		cmd := exec.Command("paplay", soundFile)
		if err := cmd.Run(); err != nil {
			cmd = exec.Command("aplay", "-q", soundFile)
			_ = cmd.Run()
		}
	}()
}

func (m *Monitor) check() error {
	charging, err := m.battery.IsCharging()
	if err != nil {
		return fmt.Errorf("read charging status: %w", err)
	}

	if charging {
		// Clear notification if charging
		if m.currentLevel != "normal" {
			log.Println("Power connected, clearing notifications")
			if err := m.notifier.Close(m.notificationID); err != nil {
				log.Printf("Failed to close notification: %v", err)
			}
			m.notificationID = 0
			m.currentLevel = "normal"
		}
		return nil
	}

	capacity, err := m.battery.Capacity()
	if err != nil {
		return fmt.Errorf("read capacity: %w", err)
	}

	log.Printf("Battery: %d%% (discharging)", capacity)

	// Determine current state
	var newLevel string
	var level BatteryLevel

	if capacity <= m.config.CriticalBattery.Threshold {
		newLevel = "critical"
		level = m.config.CriticalBattery
	} else if capacity <= m.config.LowBattery.Threshold {
		newLevel = "low"
		level = m.config.LowBattery
	} else {
		newLevel = "normal"
	}

	// Only notify on state change
	if newLevel != m.currentLevel && newLevel != "normal" {
		log.Printf("Battery level changed to: %s (%d%%)", newLevel, capacity)

		title := level.Title
		message := fmt.Sprintf(level.Message, capacity)

		// Send notification - it will replace previous one if ID exists
		id, err := m.notifier.Send(title, message, "critical", level.Icon)
		if err != nil {
			log.Printf("Failed to send notification: %v", err)
		} else {
			// Close old notification explicitly (cleaner than relying on replace)
			if m.notificationID != 0 && m.notificationID != id {
				m.notifier.Close(m.notificationID)
			}
			m.notificationID = id
		}

		m.playSound(level.Sound)
		m.currentLevel = newLevel
	} else if newLevel == "normal" && m.currentLevel != "normal" {
		// Battery recovered above thresholds
		if err := m.notifier.Close(m.notificationID); err != nil {
			log.Printf("Failed to close notification: %v", err)
		}
		m.notificationID = 0
		m.currentLevel = "normal"
	}

	return nil
}

func (m *Monitor) run() error {
	log.Printf("Starting battery monitor (poll interval: %ds)", m.config.PollInterval)
	log.Printf("Low threshold: %d%%, Critical threshold: %d%%",
		m.config.LowBattery.Threshold,
		m.config.CriticalBattery.Threshold)

	// Initial check
	if err := m.check(); err != nil {
		log.Printf("Check failed: %v", err)
	}

	ticker := time.NewTicker(time.Duration(m.config.PollInterval) * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		if err := m.check(); err != nil {
			log.Printf("Check failed: %v", err)
		}
	}

	return nil
}

func main() {
	configPath := "battery-monitor.yaml"
	if len(os.Args) > 1 {
		configPath = os.Args[1]
	}

	// Expand home directory
	if strings.HasPrefix(configPath, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			configPath = filepath.Join(home, configPath[2:])
		}
	}

	config, err := loadConfig(configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	battery, err := NewSysfsBattery()
	if err != nil {
		log.Fatalf("Failed to find battery: %v", err)
	}

	notifier, err := NewDBusNotifier(config.AppName)
	if err != nil {
		log.Fatalf("Failed to create notifier: %v", err)
	}

	monitor := &Monitor{
		config:       *config,
		battery:      battery,
		notifier:     notifier,
		currentLevel: "normal",
	}

	if err := monitor.run(); err != nil {
		log.Fatalf("Monitor failed: %v", err)
	}
}
