package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// --- Data Structures ---

// ScanRequest is the JSON payload we expect from the ESP32
type ScanRequest struct {
	UID string `json:"uid"`
}

// Session represents a completed visit (Signin + Signout)
type Session struct {
	Name        string    `json:"name"`
	UID         string    `json:"uid"`
	SignInTime  time.Time `json:"signin_time"`
	SignOutTime time.Time `json:"signout_time"`
}

// ActiveAttendee represents someone currently in the room
type ActiveAttendee struct {
	Name       string    `json:"name"`
	SignInTime time.Time `json:"signin_time"`
}

// --- Global State ---

var (
	// Lookup map for UID -> Name (loaded from file)
	userDB map[string]string

	// Map of attendees currently in room (Map[UID]SignInTime)
	currentAttendees = make(map[string]time.Time)

	// SQLite database connection
	db *sql.DB

	// Mutex to protect our maps/slices from concurrent access
	mu sync.Mutex
)

// --- Helpers ---

// initDB initializes the SQLite database and creates the sessions table
func initDB(dbPath string) error {
	var err error
	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		return err
	}

	// Create sessions table if it doesn't exist
	createTableSQL := `CREATE TABLE IF NOT EXISTS sessions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		uid TEXT NOT NULL,
		signin_time DATETIME NOT NULL,
		signout_time DATETIME NOT NULL
	);`

	_, err = db.Exec(createTableSQL)
	return err
}

// saveSessionToDB saves a completed session to the database
func saveSessionToDB(session Session) error {
	insertSQL := `INSERT INTO sessions (name, uid, signin_time, signout_time) VALUES (?, ?, ?, ?)`
	_, err := db.Exec(insertSQL, session.Name, session.UID, session.SignInTime, session.SignOutTime)
	return err
}

// loadHistoryFromDB retrieves all sessions from the database
func loadHistoryFromDB() ([]Session, error) {
	rows, err := db.Query(`SELECT name, uid, signin_time, signout_time FROM sessions ORDER BY signin_time DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sessions []Session
	for rows.Next() {
		var s Session
		var signinTime, signoutTime string
		err := rows.Scan(&s.Name, &s.UID, &signinTime, &signoutTime)
		if err != nil {
			return nil, err
		}
		s.SignInTime, _ = time.Parse(time.RFC3339, signinTime)
		s.SignOutTime, _ = time.Parse(time.RFC3339, signoutTime)
		sessions = append(sessions, s)
	}
	return sessions, rows.Err()
}

// saveCurrentAttendees saves the current attendees map to a JSON file
func saveCurrentAttendees(filename string) error {
	mu.Lock()
	defer mu.Unlock()

	data, err := json.MarshalIndent(currentAttendees, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filename, data, 0644)
}

// loadCurrentAttendees loads the current attendees from a JSON file
func loadCurrentAttendees(filename string) error {
	file, err := os.Open(filename)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer file.Close()

	bytes, err := io.ReadAll(file)
	if err != nil {
		return err
	}

	mu.Lock()
	defer mu.Unlock()
	return json.Unmarshal(bytes, &currentAttendees)
}

// loadUsers reads the users.json file into our map
func loadUsers(filename string) error {
	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	bytes, _ := io.ReadAll(file)
	userDB = make(map[string]string)
	return json.Unmarshal(bytes, &userDB)
}

// --- Handlers ---

// /scan endpoint
// Receives UID from ESP32 and processes login/logout

// handleScan processes the RFID tap
func handleScan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req ScanRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	// Identify the User
	name, exists := userDB[req.UID]
	if !exists {
		log.Printf("Unknown tag scanned: %s", req.UID)
		http.Error(w, "Unknown UID", http.StatusForbidden)
		return
	}

	// Check Logic: Are they logging IN or OUT?
	if signInTime, isInside := currentAttendees[req.UID]; isInside {
		// --- LOGOUT LOGIC ---
		signOutTime := time.Now()
		duration := signOutTime.Sub(signInTime)

		// Create a historical record
		record := Session{
			Name:        name,
			UID:         req.UID,
			SignInTime:  signInTime,
			SignOutTime: signOutTime,
		}

		// Save to database
		if err := saveSessionToDB(record); err != nil {
			log.Printf("Error saving session to database: %v", err)
		}

		// Remove from active list
		delete(currentAttendees, req.UID)

		// Persist current attendees to file
		saveCurrentAttendees("current_attendees.json")

		msg := fmt.Sprintf("Goodbye, %s! Duration: %s", name, duration.Round(time.Second))
		log.Println(msg)
		json.NewEncoder(w).Encode(map[string]string{"message": msg, "status": "out"})

	} else {
		// --- LOGIN LOGIC ---
		currentAttendees[req.UID] = time.Now()

		// Persist current attendees to file
		saveCurrentAttendees("current_attendees.json")

		msg := fmt.Sprintf("Welcome, %s!", name)
		log.Println(msg)
		json.NewEncoder(w).Encode(map[string]string{"message": msg, "status": "in"})
	}
}

// /current endpoint
// Returns list of current attendees

// handleCurrent returns a list of who is currently inside
func handleCurrent(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	defer mu.Unlock()

	var activeList []ActiveAttendee
	for uid, signinTime := range currentAttendees {
		activeList = append(activeList, ActiveAttendee{
			Name:       userDB[uid],
			SignInTime: signinTime,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(activeList)
}

// /history endpoint
// Returns historical session data

// handleHistory returns the list of completed sessions
func handleHistory(w http.ResponseWriter, r *http.Request) {
	sessions, err := loadHistoryFromDB()
	if err != nil {
		log.Printf("Error loading history from database: %v", err)
		http.Error(w, "Error loading history", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(sessions)
}

// --- Main ---

func main() {
	// Initialize SQLite database
	if err := initDB("attendance.db"); err != nil {
		log.Fatal("Could not initialize database: ", err)
	}
	defer db.Close()
	log.Println("Database initialized successfully.")

	// Load User DB
	if err := loadUsers("members.json"); err != nil {
		log.Fatal("Could not load members.json: ", err)
	}
	log.Printf("Loaded %d users from members.json.", len(userDB))

	// Load current attendees from file (if exists)
	if err := loadCurrentAttendees("current_attendees.json"); err != nil {
		log.Printf("Warning: Could not load current attendees: %v", err)
	} else {
		log.Printf("Loaded %d current attendees from file.", len(currentAttendees))
	}

	// Define Routes
	http.HandleFunc("/scan", handleScan)       // POST: ESP32 sends UID here
	http.HandleFunc("/current", handleCurrent) // GET: See who is in the room
	http.HandleFunc("/history", handleHistory) // GET: See past logs

	// Start Server
	port := ":8080"
	log.Printf("Server starting on port %s...", port)
	if err := http.ListenAndServe(port, nil); err != nil {
		log.Fatal(err)
	}
}
