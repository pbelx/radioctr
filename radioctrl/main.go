package main

//remove socat .use go options
import (
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
)

// Config represents the application configuration
type Config struct {
	ServerPort     string       `json:"server_port"`
	GamepadDevice  string       `json:"gamepad_device"`
	StationsAPIURL string       `json:"stations_api_url"`
	ButtonMappings ButtonConfig `json:"button_mappings"`
}

// ButtonConfig maps button numbers to actions
type ButtonConfig struct {
	Play       uint8 `json:"play"`
	Next       uint8 `json:"next"`
	Previous   uint8 `json:"previous"`
	Stop       uint8 `json:"stop"`
	VolumeUp   uint8 `json:"volume_up"`
	VolumeDown uint8 `json:"volume_down"`
}

// RadioStation represents a radio station with its name and stream URL
type RadioStation struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// JoystickEvent represents the structure of a joystick input event
type JoystickEvent struct {
	Time   uint32
	Value  int16
	Type   uint8
	Number uint8
}

var (
	mpvCmd     *exec.Cmd
	mpvMutex   sync.Mutex
	stations   []RadioStation
	currentIdx int
	currentVol int = 50
	config     Config
	mpvSocket  string = "/tmp/mpv-socket"
	version    string = "1.0.0"
)

// DefaultConfig returns the default configuration
func DefaultConfig() Config {
	return Config{
		ServerPort:     "8080",
		GamepadDevice:  "/dev/input/js0",
		StationsAPIURL: "https://bxmusic-stations-1111.bxmedia.workers.dev",
		ButtonMappings: ButtonConfig{
			Play:       0,
			Next:       1,
			Previous:   2,
			Stop:       3,
			VolumeDown: 6,
			VolumeUp:   7,
		},
	}
}

// LoadConfig loads configuration from file or creates default if not exists
func LoadConfig(configPath string) error {
	// If config file exists, load it
	if _, err := os.Stat(configPath); err == nil {
		data, err := ioutil.ReadFile(configPath)
		if err != nil {
			return fmt.Errorf("error reading config file: %v", err)
		}
		if err := json.Unmarshal(data, &config); err != nil {
			return fmt.Errorf("error parsing config file: %v", err)
		}
		return nil
	}

	// Create default config
	config = DefaultConfig()
	data, err := json.MarshalIndent(config, "", "    ")
	if err != nil {
		return fmt.Errorf("error creating default config: %v", err)
	}

	// Create config directory if it doesn't exist
	configDir := filepath.Dir(configPath)
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("error creating config directory: %v", err)
	}

	// Write default config file
	if err := ioutil.WriteFile(configPath, data, 0644); err != nil {
		return fmt.Errorf("error writing default config: %v", err)
	}

	log.Printf("Created default config file at: %s\n", configPath)
	return nil
}

// FetchRadioStations fetches the list of radio stations from an external API
func FetchRadioStations(apiURL string) ([]RadioStation, error) {
	client := &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
		},
	}

	resp, err := client.Get(apiURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var fetchedStations []RadioStation
	if err := json.Unmarshal(body, &fetchedStations); err != nil {
		return nil, err
	}

	return fetchedStations, nil
}

// SendMPVCommand sends a command to the running mpv process via Unix domain socket
func SendMPVCommand(command string) error {
	// Ensure command ends with newline
	if command[len(command)-1] != '\n' {
		command += "\n"
	}

	// Connect to MPV's Unix domain socket
	conn, err := net.Dial("unix", mpvSocket)
	if err != nil {
		return fmt.Errorf("failed to connect to MPV socket: %v", err)
	}
	defer conn.Close()

	// Write command to socket
	if _, err := conn.Write([]byte(command)); err != nil {
		return fmt.Errorf("failed to write to MPV socket: %v", err)
	}

	// Read response
	response := make([]byte, 1024)
	_, err = conn.Read(response)
	if err != nil && err != io.EOF {
		log.Printf("Warning: couldn't read MPV response: %v", err)
	}

	return nil
}

// StartMPV starts the mpv process with the given stream URL
func StartMPV(url string) error {
	mpvMutex.Lock()
	defer mpvMutex.Unlock()

	// Kill existing mpv process if it's running
	if mpvCmd != nil && mpvCmd.Process != nil {
		mpvCmd.Process.Kill()
		mpvCmd.Wait()
	}

	// Remove existing socket if it exists
	os.Remove(mpvSocket)

	// Start a new mpv process
	mpvCmd = exec.Command("mpv", "--no-video", "--idle=yes", fmt.Sprintf("--input-ipc-server=%s", mpvSocket), url)
	if err := mpvCmd.Start(); err != nil {
		return fmt.Errorf("failed to start mpv: %v", err)
	}

	// Wait for socket to be created
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(mpvSocket); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Set initial volume
	time.Sleep(500 * time.Millisecond)
	if err := AdjustVolume(0); err != nil {
		log.Printf("Warning: couldn't set initial volume: %v", err)
	}

	return nil
}

// PlayNextStation switches to the next station in the list
func PlayNextStation() error {
	currentIdx = (currentIdx + 1) % len(stations)
	log.Printf("Playing next station: %s\n", stations[currentIdx].Name)
	return StartMPV(stations[currentIdx].URL)
}

// PlayPrevStation switches to the previous station in the list
func PlayPrevStation() error {
	currentIdx = (currentIdx - 1 + len(stations)) % len(stations)
	log.Printf("Playing previous station: %s\n", stations[currentIdx].Name)
	return StartMPV(stations[currentIdx].URL)
}

// AdjustVolume changes the volume by a given delta
func AdjustVolume(delta int) error {
	currentVol += delta
	if currentVol < 0 {
		currentVol = 0
	} else if currentVol > 100 {
		currentVol = 100
	}

	command := fmt.Sprintf(`{ "command": ["set_property", "volume", %d] }`, currentVol)
	return SendMPVCommand(command)
}

// StopPlayer sends the stop command to MPV
func StopPlayer() error {
	log.Println("Stopping playback")
	return SendMPVCommand(`{ "command": ["stop"] }`)
}

// StartGamepadListener starts listening for gamepad events
func StartGamepadListener(devicePath string, quit chan struct{}) error {
	device, err := os.OpenFile(devicePath, os.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("error opening device: %v", err)
	}
	defer device.Close()

	log.Printf("Gamepad listener started on device: %s\n", devicePath)
	event := JoystickEvent{}
	for {
		select {
		case <-quit:
			return nil
		default:
			err := binary.Read(device, binary.LittleEndian, &event)
			if err != nil {
				return fmt.Errorf("error reading event: %v", err)
			}
			processGamepadEvent(event)
		}
	}
}

// processGamepadEvent handles gamepad events and triggers commands
func processGamepadEvent(event JoystickEvent) {
	if event.Type != 1 || event.Value != 1 {
		return
	}

	var err error
	switch event.Number {
	case config.ButtonMappings.Play:
		err = StartMPV(stations[currentIdx].URL)
	case config.ButtonMappings.Next:
		err = PlayNextStation()
	case config.ButtonMappings.Previous:
		err = PlayPrevStation()
	case config.ButtonMappings.Stop:
		err = StopPlayer()
	case config.ButtonMappings.VolumeUp:
		err = AdjustVolume(10)
	case config.ButtonMappings.VolumeDown:
		err = AdjustVolume(-10)
	}

	if err != nil {
		log.Printf("Error executing command for button %d: %v\n", event.Number, err)
	}
}

func setupServer() *gin.Engine {
	r := gin.Default()

	// Add version endpoint
	r.GET("/version", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"version": version})
	})

	r.GET("/stations", func(c *gin.Context) {
		c.JSON(http.StatusOK, stations)
	})

	r.GET("/status", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"current_station": stations[currentIdx].Name,
			"volume":          currentVol,
		})
	})

	r.POST("/play", func(c *gin.Context) {
		if err := StartMPV(stations[currentIdx].URL); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": "Playing station"})
	})

	r.POST("/next", func(c *gin.Context) {
		if err := PlayNextStation(); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("Playing: %s", stations[currentIdx].Name)})
	})

	r.POST("/prev", func(c *gin.Context) {
		if err := PlayPrevStation(); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("Playing: %s", stations[currentIdx].Name)})
	})

	r.POST("/stop", func(c *gin.Context) {
		if err := StopPlayer(); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": "Playback stopped"})
	})

	r.POST("/volup", func(c *gin.Context) {
		if err := AdjustVolume(10); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("Volume: %d", currentVol)})
	})

	r.POST("/voldown", func(c *gin.Context) {
		if err := AdjustVolume(-10); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": fmt.Sprintf("Volume: %d", currentVol)})
	})

	return r
}

func main() {
	log.Printf("Radiopad v%s starting...\n", version)

	// Parse command line flags
	configPath := flag.String("config", filepath.Join(os.Getenv("HOME"), ".config", "radiopad", "config.json"), "Path to config file")
	serverPort := flag.String("port", "", "Server port (overrides config file)")
	gamepadDevice := flag.String("gamepad", "", "Gamepad device path (overrides config file)")
	stationsAPI := flag.String("api", "", "Stations API URL (overrides config file)")
	socketPath := flag.String("socket", "/tmp/mpv-socket", "MPV socket path")
	flag.Parse()

	// Update MPV socket path
	mpvSocket = *socketPath

	// Load configuration
	if err := LoadConfig(*configPath); err != nil {
		log.Fatalf("Error loading config: %v", err)
	}

	// Override config with command line arguments if provided
	if *serverPort != "" {
		config.ServerPort = *serverPort
	}
	if *gamepadDevice != "" {
		config.GamepadDevice = *gamepadDevice
	}
	if *stationsAPI != "" {
		config.StationsAPIURL = *stationsAPI
	}

	// Fetch radio stations
	var err error
	stations, err = FetchRadioStations(config.StationsAPIURL)
	if err != nil {
		log.Fatalf("Failed to fetch radio stations: %v", err)
	}
	log.Printf("Loaded %d radio stations\n", len(stations))

	// Setup signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Create quit channel for gamepad listener
	quit := make(chan struct{})

	// Start gamepad listener in a goroutine
	go func() {
		if err := StartGamepadListener(config.GamepadDevice, quit); err != nil {
			log.Printf("Gamepad listener error: %v\n", err)
		}
	}()

	// Setup and start HTTP server
	r := setupServer()
	log.Printf("Server starting on port %s\n", config.ServerPort)
	if err := r.Run(":" + config.ServerPort); err != nil {
		log.Fatalf("Server failed to start: %v", err)
	}
}
