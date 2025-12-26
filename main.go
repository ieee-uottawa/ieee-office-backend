package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// --- Constants ---

const (
	dataFolder               = "data/"
	currentAttendeesFilePath = dataFolder + "current_attendees.json"
	membersFilePath          = dataFolder + "members.json"
	databaseFilePath         = dataFolder + "attendance.db"
)

// --- Data Structures ---

// ScanRequest is the JSON payload we expect from the ESP32
type ScanRequest struct {
	UID string `json:"uid"`
}

// Session represents a completed visit (Signin + Signout)
type Session struct {
	Name        string    `json:"name"`
	SignInTime  time.Time `json:"signin_time"`
	SignOutTime time.Time `json:"signout_time"`
}

// ActiveAttendee represents someone currently in the room
type ActiveAttendee struct {
	Name       string    `json:"name"`
	SignInTime time.Time `json:"signin_time"`
}

// ScanEvent captures a single scan with timestamp (most recent 10 kept in memory)
type ScanEvent struct {
	UID  string    `json:"uid"`
	Time time.Time `json:"time"`
}

// Member represents a person with a registered RFID tag
type Member struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
	DiscordID string `json:"discord_id"`
}

// CreateMemberRequest is the payload to create a member
type CreateMemberRequest struct {
	Name      string `json:"name"`
	UID       string `json:"uid"`
	DiscordID string `json:"discord_id"`
}

// --- Global State ---

var (
	// In-memory cache for UID -> Member (loaded from DB at startup)
	userDB map[string]Member

	// In-memory ring buffer storing last 10 scan events
	scanHistory []ScanEvent

	// Map of attendees currently in room (Map[UID]SignInTime)
	currentAttendees = make(map[string]time.Time)

	// SQLite database connection
	db *sql.DB

	// Mutex to protect our maps/slices from concurrent access
	mu sync.RWMutex

	// API keys for client authentication
	validAPIKeys map[string]bool // Map of valid API keys (loaded from env)
)

// --- Helpers ---

// corsMiddleware adds CORS headers to allow cross-origin requests
func corsMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Get allowed origins from environment variable, default to "*"
		allowedOrigins := os.Getenv("ALLOWED_ORIGINS")
		if allowedOrigins == "" {
			allowedOrigins = "*"
		}

		w.Header().Set("Access-Control-Allow-Origin", allowedOrigins)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-API-Key")
		w.Header().Set("Access-Control-Max-Age", "3600")

		// Handle preflight OPTIONS request
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next(w, r)
	}
}

// apiKeyMiddleware validates API key before processing requests
func apiKeyMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Health check endpoint is always allowed
		if r.URL.Path == "/health" {
			next(w, r)
			return
		}

		// Get API key from X-API-Key header
		apiKey := r.Header.Get("X-API-Key")

		// If no API keys configured, allow all requests
		if len(validAPIKeys) == 0 {
			next(w, r)
			return
		}

		// Validate API key
		if apiKey == "" || !validAPIKeys[apiKey] {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{
				"error": "missing or invalid API key",
			})
			return
		}

		next(w, r)
	}
}

// initDB initializes the SQLite database and creates the members and sessions tables
func initDB() error {
	var err error
	db, err = sql.Open("sqlite", databaseFilePath)
	if err != nil {
		return err
	}

	// Set WAL journal mode for better concurrency
	if _, err := db.Exec(`PRAGMA journal_mode = WAL;`); err != nil {
		return fmt.Errorf("failed to enable WAL: %w", err)
	}
	if _, err := db.Exec(`PRAGMA busy_timeout = 5000;`); err != nil {
		return fmt.Errorf("failed to set busy timeout: %w", err)
	}

	// Enable foreign keys
	if _, err = db.Exec(`PRAGMA foreign_keys = ON;`); err != nil {
		return err
	}

	// Create members table
	createMembersSQL := `CREATE TABLE IF NOT EXISTS members (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		uid TEXT NOT NULL UNIQUE,
		discord_id TEXT NOT NULL
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
		SELECT m.name, s.signin_time, s.signout_time
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
		err := rows.Scan(&s.Name, &signinTime, &signoutTime)
		if err != nil {
			return nil, err
		}
		s.SignInTime, err = time.Parse(time.RFC3339, signinTime)
		if err != nil {
			return nil, err
		}
		s.SignOutTime, err = time.Parse(time.RFC3339, signoutTime)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, s)
	}
	return sessions, rows.Err()
}

// loadAPIKeys loads API keys from environment variables and returns a map of valid keys
func loadAPIKeys() map[string]bool {
	keys := make(map[string]bool)

	// Load individual API keys for specific clients
	if scannerKey := os.Getenv("SCANNER_API_KEY"); scannerKey != "" {
		keys[scannerKey] = true
	}
	if botKey := os.Getenv("DISCORD_BOT_API_KEY"); botKey != "" {
		keys[botKey] = true
	}

	// Load comma-separated list of API keys from API_KEYS environment variable
	if apiKeys := os.Getenv("API_KEYS"); apiKeys != "" {
		for _, key := range strings.Split(apiKeys, ",") {
			key = strings.TrimSpace(key)
			if key != "" {
				keys[key] = true
			}
		}
	}

	return keys
}

// saveCurrentAttendees saves the current attendees map to a JSON file
func saveCurrentAttendees() error {
	// Snapshot under read lock to avoid holding lock during I/O
	mu.RLock()
	copyMap := make(map[string]time.Time, len(currentAttendees))
	for k, v := range currentAttendees {
		copyMap[k] = v
	}
	mu.RUnlock()

	data, err := json.MarshalIndent(copyMap, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(currentAttendeesFilePath, data, 0644)
}

// loadCurrentAttendees loads the current attendees from a JSON file
func loadCurrentAttendees() error {
	file, err := os.Open(currentAttendeesFilePath)
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

	var loaded map[string]time.Time
	if err := json.Unmarshal(bytes, &loaded); err != nil {
		return err
	}
	mu.Lock()
	currentAttendees = loaded
	mu.Unlock()
	return nil
}

// loadMembersIntoCache populates userDB from the members table
func loadMembersIntoCache() error {
	rows, err := db.Query(`SELECT id, name, uid, discord_id FROM members`)
	if err != nil {
		return err
	}
	defer rows.Close()

	cache := make(map[string]Member)
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.ID, &m.Name, &m.UID, &m.DiscordID); err != nil {
			return err
		}
		cache[m.UID] = m
	}
	if err := rows.Err(); err != nil {
		return err
	}

	mu.Lock()
	userDB = cache
	mu.Unlock()
	return nil
}

// recordScanEvent appends a scan to history while keeping only the last 10 entries
func recordScanEvent(uid string, t time.Time) {
	mu.Lock()
	scanHistory = append(scanHistory, ScanEvent{UID: uid, Time: t})
	if len(scanHistory) > 10 {
		scanHistory = scanHistory[len(scanHistory)-10:]
	}
	mu.Unlock()
}

// performSignIn signs in a member and returns message
func performSignIn(member Member) (string, error) {
	mu.Lock()
	currentAttendees[member.UID] = time.Now()
	mu.Unlock()

	if err := saveCurrentAttendees(); err != nil {
		return "", err
	}

	msg := fmt.Sprintf("Welcome, %s!", member.Name)
	return msg, nil
}

// performSignOut signs out a member and returns message
func performSignOut(member Member, signInTime time.Time) (string, error) {
	mu.Lock()
	delete(currentAttendees, member.UID)
	mu.Unlock()

	signOutTime := time.Now()
	if err := saveSessionToDB(member.ID, signInTime, signOutTime); err != nil {
		return "", err
	}

	if err := saveCurrentAttendees(); err != nil {
		return "", err
	}

	duration := signOutTime.Sub(signInTime)
	msg := fmt.Sprintf("Goodbye, %s! Duration: %s", member.Name, duration.Round(time.Second))
	return msg, nil
}

// startNightlyCleanup runs a goroutine that forces sign-out of all attendees at 4:00 AM daily
func startNightlyCleanup() {
	for {
		now := time.Now()
		// Calculate duration until next 4:00 AM
		next := time.Date(now.Year(), now.Month(), now.Day(), 4, 0, 0, 0, now.Location())
		if next.Before(now) {
			next = next.Add(24 * time.Hour)
		}
		timer := time.NewTimer(next.Sub(now))

		<-timer.C

		// Copy and clear under lock, then persist outside
		mu.Lock()
		cnt := len(currentAttendees)
		if cnt > 0 {
			log.Printf("Nightly Cleanup: Force signing out %d people", cnt)
			toSignOut := currentAttendees
			currentAttendees = make(map[string]time.Time)
			mu.Unlock()

			for uid, signin := range toSignOut {
				saveSessionToDB(userDB[uid].ID, signin, time.Now())
			}
		}
	}
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

	// Record scan event before processing sign-in/out
	eventTime := time.Now()
	recordScanEvent(req.UID, eventTime)

	// Identify the Member (read lock)
	mu.RLock()
	member, exists := userDB[req.UID]
	mu.RUnlock()
	if !exists {
		log.Printf("Unknown tag scanned: %s", req.UID)
		http.Error(w, "Unknown UID", http.StatusForbidden)
		return
	}

	// Check Logic: Are they logging IN or OUT?
	mu.RLock()
	signInTime, isInside := currentAttendees[req.UID]
	mu.RUnlock()
	if isInside {
		// --- LOGOUT LOGIC ---
		msg, err := performSignOut(member, signInTime)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		log.Println(msg)
		json.NewEncoder(w).Encode(map[string]string{"message": msg, "status": "out"})

	} else {
		// --- LOGIN LOGIC ---
		msg, err := performSignIn(member)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		log.Println(msg)
		json.NewEncoder(w).Encode(map[string]string{"message": msg, "status": "in"})
	}
}

// /current endpoint
// Returns list of current attendees

// handleCurrent returns a list of who is currently inside, sorted by sign-in time (oldest first)
func handleCurrent(w http.ResponseWriter, r *http.Request) {
	mu.RLock()
	activeList := make([]ActiveAttendee, 0)
	for uid, signinTime := range currentAttendees {
		activeList = append(activeList, ActiveAttendee{
			Name:       userDB[uid].Name,
			SignInTime: signinTime,
		})
	}
	mu.RUnlock()

	// Sort by sign-in time (oldest first)
	sort.Slice(activeList, func(i, j int) bool {
		return activeList[i].SignInTime.Before(activeList[j].SignInTime)
	})

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

// handleScanHistory returns the most recent 10 scan events (newest first)
func handleScanHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	mu.RLock()
	history := make([]ScanEvent, len(scanHistory))
	copy(history, scanHistory)
	mu.RUnlock()

	// Reverse to return newest first without mutating shared slice
	for i, j := 0, len(history)-1; i < j; i, j = i+1, j-1 {
		history[i], history[j] = history[j], history[i]
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(history)
}

// handleMember handles updating or deleting a single member by ID (PUT/DELETE)
func handleMember(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut && r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract member ID from URL path (e.g., /members/123)
	path := r.URL.Path
	idStr := strings.TrimPrefix(path, "/members/")
	if idStr == "" || idStr == path {
		http.Error(w, "Member ID required in path", http.StatusBadRequest)
		return
	}

	var id int64
	if _, err := fmt.Sscanf(idStr, "%d", &id); err != nil {
		http.Error(w, "Invalid member ID", http.StatusBadRequest)
		return
	}

	// Handle DELETE request
	if r.Method == http.MethodDelete {
		// Check if member exists and get UID before deletion
		var uid string
		err := db.QueryRow(`SELECT uid FROM members WHERE id = ?`, id).Scan(&uid)
		if err == sql.ErrNoRows {
			http.Error(w, "Member not found", http.StatusNotFound)
			return
		} else if err != nil {
			log.Printf("Error querying member: %v", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		// Check if member is currently signed in
		mu.RLock()
		_, isSignedIn := currentAttendees[uid]
		mu.RUnlock()

		if isSignedIn {
			http.Error(w, "Cannot delete member who is currently signed in", http.StatusConflict)
			return
		}

		// Delete from database (CASCADE will delete related sessions)
		result, err := db.Exec(`DELETE FROM members WHERE id = ?`, id)
		if err != nil {
			log.Printf("Error deleting member: %v", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		rowsAffected, _ := result.RowsAffected()
		if rowsAffected == 0 {
			http.Error(w, "Member not found", http.StatusNotFound)
			return
		}

		// Reload cache to reflect deletion
		if err := loadMembersIntoCache(); err != nil {
			log.Printf("Warning: Failed to reload members cache: %v", err)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"message": "Member deleted successfully"})
		return
	}

	// Parse update request
	var req struct {
		Name      string `json:"name"`
		UID       string `json:"uid"`
		DiscordID string `json:"discord_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	req.Name = strings.TrimSpace(req.Name)
	req.UID = strings.TrimSpace(req.UID)
	req.DiscordID = strings.TrimSpace(req.DiscordID)

	if req.Name == "" || req.UID == "" || req.DiscordID == "" {
		http.Error(w, "name, uid, and discord_id are required", http.StatusBadRequest)
		return
	}

	// Update in database
	result, err := db.Exec(`UPDATE members SET name = ?, uid = ?, discord_id = ? WHERE id = ?`,
		req.Name, req.UID, req.DiscordID, id)
	if err != nil {
		// Handle unique constraint on uid
		if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "unique") {
			http.Error(w, "UID already exists", http.StatusConflict)
			return
		}
		log.Printf("Error updating member: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	rowsAffected, _ := result.RowsAffected()
	if rowsAffected == 0 {
		http.Error(w, "Member not found", http.StatusNotFound)
		return
	}

	// Reload cache to reflect changes
	if err := loadMembersIntoCache(); err != nil {
		log.Printf("Warning: Failed to reload members cache: %v", err)
	}

	member := Member{ID: id, Name: req.Name, UID: req.UID, DiscordID: req.DiscordID}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(member)
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
		req.DiscordID = strings.TrimSpace(req.DiscordID)
		if req.Name == "" || req.UID == "" || req.DiscordID == "" {
			http.Error(w, "name, uid, and discord_id are required", http.StatusBadRequest)
			return
		}

		// Insert into DB
		res, err := db.Exec(`INSERT INTO members (name, uid, discord_id) VALUES (?, ?, ?)`, req.Name, req.UID, req.DiscordID)
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

		loadMembersIntoCache()

		member := Member{ID: id, Name: req.Name, UID: req.UID, DiscordID: req.DiscordID}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(member)

	case http.MethodGet:
		// Return list of members
		rows, err := db.Query(`SELECT id, name, uid, discord_id FROM members`)
		if err != nil {
			log.Printf("Error querying members: %v", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		var members []Member
		for rows.Next() {
			var m Member
			if err := rows.Scan(&m.ID, &m.Name, &m.UID, &m.DiscordID); err != nil {
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

// handleCount returns the number of current attendees
func handleCount(w http.ResponseWriter, r *http.Request) {
	mu.RLock()
	count := len(currentAttendees)
	mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]int{"count": count})
}

// handleHealth returns a simple health check response
func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

// handleSignoutAll signs out all current attendees
func handleSignoutAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Copy current attendees and clear under lock
	mu.Lock()
	count := len(currentAttendees)
	toSignOut := make(map[string]time.Time, count)
	for uid, t := range currentAttendees {
		toSignOut[uid] = t
	}
	currentAttendees = make(map[string]time.Time)
	mu.Unlock()

	// Persist cleared state
	_ = saveCurrentAttendees()

	// Save sessions for those who were signed out
	for uid, signinTime := range toSignOut {
		signOutTime := time.Now()
		if err := saveSessionToDB(userDB[uid].ID, signinTime, signOutTime); err != nil {
			log.Printf("Error saving session to database during signout all: %v", err)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	msg := fmt.Sprintf("Signed out all attendees (%d total).", count)
	log.Println(msg)
	json.NewEncoder(w).Encode(map[string]string{"message": msg})
}

func handleSignInWithDiscordID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse Discord ID from request
	var req struct {
		DiscordID string `json:"discord_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	// Find member by Discord ID (read lock)
	mu.RLock()
	var member Member
	found := false
	for _, m := range userDB {
		if m.DiscordID == req.DiscordID {
			member = m // copy
			found = true
			break
		}
	}
	mu.RUnlock()
	if !found {
		http.Error(w, "Member not found", http.StatusNotFound)
		return
	}

	// Check if already signed in and sign in atomically
	mu.Lock()
	if _, isInside := currentAttendees[member.UID]; isInside {
		mu.Unlock()
		http.Error(w, "Member already signed in", http.StatusConflict)
		return
	}
	mu.Unlock()
	msg, err := performSignIn(member)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Println(msg)
	json.NewEncoder(w).Encode(map[string]string{"message": msg, "status": "in"})
}

func handleSignOutWithDiscordID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse Discord ID from request
	var req struct {
		DiscordID string `json:"discord_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	// Find member by Discord ID (read lock)
	mu.RLock()
	var member Member
	found := false
	for _, m := range userDB {
		if m.DiscordID == req.DiscordID {
			member = m // copy
			found = true
			break
		}
	}
	mu.RUnlock()
	if !found {
		http.Error(w, "Member not found", http.StatusNotFound)
		return
	}

	// Check if signed in and remove under lock
	mu.Lock()
	signInTime, isInside := currentAttendees[member.UID]
	if !isInside {
		mu.Unlock()
		http.Error(w, "Member not signed in", http.StatusConflict)
		return
	}
	mu.Unlock()
	msg, err := performSignOut(member, signInTime)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Println(msg)
	json.NewEncoder(w).Encode(map[string]string{"message": msg, "status": "out"})
}

// Export members in database to members.json file
func handleExportMembers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	rows, err := db.Query(`SELECT id, name, uid, discord_id FROM members`)
	if err != nil {
		log.Printf("Error querying members for export: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var members []Member
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.ID, &m.Name, &m.UID, &m.DiscordID); err != nil {
			log.Printf("Error scanning member row for export: %v", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		members = append(members, m)
	}

	data, err := json.MarshalIndent(members, "", "  ")
	if err != nil {
		log.Printf("Error marshaling members for export: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	if err := os.WriteFile(membersFilePath, data, 0644); err != nil {
		log.Printf("Error writing members to file: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	msg := fmt.Sprintf("Exported %d members to %s", len(members), membersFilePath)
	log.Println(msg)
	json.NewEncoder(w).Encode(map[string]string{"message": msg})
}

// Import members from members.json file to database
func handleImportMembers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	data, err := os.ReadFile(membersFilePath)
	if err != nil {
		log.Printf("Error reading members file for import: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	var members []Member
	if err := json.Unmarshal(data, &members); err != nil {
		log.Printf("Error unmarshaling members for import: %v", err)
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	importedCount := 0
	for _, m := range members {
		_, err := db.Exec(`INSERT OR IGNORE INTO members (name, uid, discord_id) VALUES (?, ?, ?)`, m.Name, m.UID, m.DiscordID)
		if err != nil {
			log.Printf("Error inserting member during import: %v", err)
			continue
		}
		importedCount++
	}

	loadMembersIntoCache()

	msg := fmt.Sprintf("Imported %d members from %s", importedCount, membersFilePath)
	log.Println(msg)
	json.NewEncoder(w).Encode(map[string]string{"message": msg})
}

// --- Main ---

func main() {
	// Create data folder if it doesn't exist
	if _, err := os.Stat(dataFolder); os.IsNotExist(err) {
		if err := os.Mkdir(dataFolder, 0755); err != nil {
			log.Fatal("Could not create data folder: ", err)
		}
	}

	// Initialize SQLite database
	if err := initDB(); err != nil {
		log.Fatal("Could not initialize database: ", err)
	}
	defer db.Close()
	log.Println("Database initialized successfully.")

	// Load members into memory cache from database
	if err := loadMembersIntoCache(); err != nil {
		log.Fatal("Could not load members: ", err)
	}
	mu.RLock()
	loadedMembers := len(userDB)
	mu.RUnlock()
	log.Printf("Loaded %d members into cache.", loadedMembers)

	// Load current attendees from file (if exists)
	if err := loadCurrentAttendees(); err != nil {
		log.Printf("Warning: Could not load current attendees: %v", err)
	} else {
		log.Printf("Loaded %d current attendees from file.", len(currentAttendees))
	}

	// Load API keys from environment
	validAPIKeys = loadAPIKeys()
	if len(validAPIKeys) > 0 {
		log.Printf("Loaded %d API key(s) for authentication.", len(validAPIKeys))
	} else {
		log.Println("Warning: No API keys configured. All endpoints are public. Set SCANNER_API_KEY, DISCORD_BOT_API_KEY, or API_KEYS environment variables for security.")
	}

	// Define Routes with CORS and API key middleware
	wrapRoute := func(handler http.HandlerFunc) http.HandlerFunc {
		return corsMiddleware(apiKeyMiddleware(handler))
	}

	http.HandleFunc("/scan", wrapRoute(handleScan))                             // POST: ESP32 sends UID here
	http.HandleFunc("/current", wrapRoute(handleCurrent))                       // GET: See who is in the room
	http.HandleFunc("/history", wrapRoute(handleHistory))                       // GET: See past logs
	http.HandleFunc("/scan-history", wrapRoute(handleScanHistory))              // GET: See recent scan events
	http.HandleFunc("/members/", wrapRoute(handleMember))                       // PUT: update member by ID
	http.HandleFunc("/members", wrapRoute(handleMembers))                       // GET: list members, POST: create member
	http.HandleFunc("/count", wrapRoute(handleCount))                           // GET: get current attendee count
	http.HandleFunc("/health", corsMiddleware(handleHealth))                    // GET: health check (no API key needed)
	http.HandleFunc("/sign-out-all", wrapRoute(handleSignoutAll))               // POST: sign out all attendees
	http.HandleFunc("/sign-in-discord", wrapRoute(handleSignInWithDiscordID))   // POST: sign in with Discord ID
	http.HandleFunc("/sign-out-discord", wrapRoute(handleSignOutWithDiscordID)) // POST: sign out with Discord ID
	http.HandleFunc("/export-members", wrapRoute(handleExportMembers))          // GET: export members as json file
	http.HandleFunc("/import-members", wrapRoute(handleImportMembers))          // POST: import members from json file

	// Start Nightly Cleanup Goroutine
	go startNightlyCleanup()

	// Start Server
	port := ":8080"
	log.Printf("Server starting on port %s...", port)
	if err := http.ListenAndServe(port, nil); err != nil {
		log.Fatal(err)
	}
}
