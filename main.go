package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
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

// Member represents a person with a registered RFID tag
type Member struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	UID  string `json:"uid"`
}

// CreateMemberRequest is the payload to create a member
type CreateMemberRequest struct {
	Name string `json:"name"`
	UID  string `json:"uid"`
}

// --- Global State ---

var (
	// In-memory cache for UID -> Member (loaded from DB at startup)
	userDB map[string]Member

	// Map of attendees currently in room (Map[UID]SignInTime)
	currentAttendees = make(map[string]time.Time)

	// SQLite database connection
	db *sql.DB

	// Mutex to protect our maps/slices from concurrent access
	mu sync.Mutex
)

// --- Helpers ---

// initDB initializes the SQLite database and creates the members and sessions tables
func initDB(dbPath string) error {
	var err error
	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		return err
	}

	// Enable foreign keys
	if _, err = db.Exec(`PRAGMA foreign_keys = ON;`); err != nil {
		return err
	}

	// Create members table
	createMembersSQL := `CREATE TABLE IF NOT EXISTS members (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		uid TEXT NOT NULL UNIQUE
	);`

	if _, err = db.Exec(createMembersSQL); err != nil {
		return err
	}

	// Create sessions table referencing members
	createSessionsSQL := `CREATE TABLE IF NOT EXISTS sessions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		member_id INTEGER NOT NULL,
		signin_time TEXT NOT NULL,
		signout_time TEXT NOT NULL,
		FOREIGN KEY(member_id) REFERENCES members(id) ON DELETE CASCADE
	);`

	_, err = db.Exec(createSessionsSQL)
	return err
}

// saveSessionToDB saves a completed session to the database using member_id
func saveSessionToDB(memberID int64, signin time.Time, signout time.Time) error {
	insertSQL := `INSERT INTO sessions (member_id, signin_time, signout_time) VALUES (?, ?, ?)`
	_, err := db.Exec(insertSQL, memberID, signin.Format(time.RFC3339), signout.Format(time.RFC3339))
	return err
}

// loadHistoryFromDB retrieves all sessions from the database
func loadHistoryFromDB() ([]Session, error) {
	rows, err := db.Query(`
		SELECT m.name, m.uid, s.signin_time, s.signout_time
		FROM sessions s
		JOIN members m ON m.id = s.member_id
		ORDER BY s.signin_time DESC`)
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

// seedMembersFromFile loads members from a JSON file into the DB if they don't exist, then populates in-memory cache
func seedMembersFromFile(filename string) error {
	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	// Expect a map of UID -> Name in the JSON file
	var fileMembers map[string]string
	bytes, _ := io.ReadAll(file)
	if err := json.Unmarshal(bytes, &fileMembers); err != nil {
		return err
	}

	// Insert or ignore into members table
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	// if UID already exists, do nothing
	stmt, err := tx.Prepare(`INSERT INTO members (name, uid) VALUES (?, ?) ON CONFLICT(uid) DO NOTHING`)
	if err != nil {
		tx.Rollback()
		return err
	}
	for uid, name := range fileMembers {
		if _, err := stmt.Exec(name, uid); err != nil {
			tx.Rollback()
			return err
		}
	}
	if err := stmt.Close(); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	return loadMembersIntoCache()
}

// loadMembersIntoCache populates userDB from the members table
func loadMembersIntoCache() error {
	rows, err := db.Query(`SELECT id, name, uid FROM members`)
	if err != nil {
		return err
	}
	defer rows.Close()

	cache := make(map[string]Member)
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.ID, &m.Name, &m.UID); err != nil {
			return err
		}
		cache[m.UID] = m
	}
	if err := rows.Err(); err != nil {
		return err
	}

	userDB = cache
	return nil
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

	// Identify the Member
	member, exists := userDB[req.UID]
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
		// Save to database
		if err := saveSessionToDB(member.ID, signInTime, signOutTime); err != nil {
			log.Printf("Error saving session to database: %v", err)
		}

		// Remove from active list
		delete(currentAttendees, req.UID)

		// Persist current attendees to file
		saveCurrentAttendees("current_attendees.json")

		msg := fmt.Sprintf("Goodbye, %s! Duration: %s", member.Name, duration.Round(time.Second))
		log.Println(msg)
		json.NewEncoder(w).Encode(map[string]string{"message": msg, "status": "out"})

	} else {
		// --- LOGIN LOGIC ---
		currentAttendees[req.UID] = time.Now()

		// Persist current attendees to file
		saveCurrentAttendees("current_attendees.json")

		msg := fmt.Sprintf("Welcome, %s!", member.Name)
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
			Name:       userDB[uid].Name,
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

// handleMembers supports POST to create a new member and GET to list members
func handleMembers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var req CreateMemberRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid JSON", http.StatusBadRequest)
			return
		}
		req.Name = strings.TrimSpace(req.Name)
		req.UID = strings.TrimSpace(req.UID)
		if req.Name == "" || req.UID == "" {
			http.Error(w, "name and uid are required", http.StatusBadRequest)
			return
		}

		// Insert into DB
		res, err := db.Exec(`INSERT INTO members (name, uid) VALUES (?, ?)`, req.Name, req.UID)
		if err != nil {
			// Handle unique constraint on uid
			if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "unique") {
				http.Error(w, "UID already exists", http.StatusConflict)
				return
			}
			log.Printf("Error inserting member: %v", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		id, _ := res.LastInsertId()

		// Update in-memory cache
		mu.Lock()
		userDB[req.UID] = Member{ID: id, Name: req.Name, UID: req.UID}
		mu.Unlock()

		// Update members.json
		fileMembers := make(map[string]string)
		for uid, member := range userDB {
			fileMembers[uid] = member.Name
		}
		data, _ := json.MarshalIndent(fileMembers, "", "  ")
		if err := os.WriteFile("members.json", data, 0644); err != nil {
			log.Printf("Warning: could not update members.json: %v", err)
		}

		member := Member{ID: id, Name: req.Name, UID: req.UID}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(member)

	case http.MethodGet:
		// Return list of members
		rows, err := db.Query(`SELECT id, name, uid FROM members`)
		if err != nil {
			log.Printf("Error querying members: %v", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		var members []Member
		for rows.Next() {
			var m Member
			if err := rows.Scan(&m.ID, &m.Name, &m.UID); err != nil {
				log.Printf("Error scanning member row: %v", err)
				http.Error(w, "Internal server error", http.StatusInternalServerError)
				return
			}
			members = append(members, m)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(members)

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// --- Main ---

func main() {
	// Initialize SQLite database
	if err := initDB("attendance.db"); err != nil {
		log.Fatal("Could not initialize database: ", err)
	}
	defer db.Close()
	log.Println("Database initialized successfully.")

	// Seed members from file and load into memory cache
	if err := seedMembersFromFile("members.json"); err != nil {
		log.Fatal("Could not seed/load members: ", err)
	}
	log.Printf("Loaded %d members into cache.", len(userDB))

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
	http.HandleFunc("/members", handleMembers) // GET: list members, POST: create member

	// Start Server
	port := ":8080"
	log.Printf("Server starting on port %s...", port)
	if err := http.ListenAndServe(port, nil); err != nil {
		log.Fatal(err)
	}
}
