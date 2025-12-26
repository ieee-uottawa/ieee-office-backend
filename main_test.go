package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// setupTest resets global state for a clean testing environment
func setupTest() {
	// Ensure data folder exists for file-based helpers used by handlers
	if err := os.MkdirAll(dataFolder, 0o755); err != nil {
		panic(err)
	}

	// Reset User DB
	userDB = map[string]Member{
		"TEST_UID_1": {ID: 1, Name: "Alice", UID: "TEST_UID_1", DiscordID: "111111111"},
		"TEST_UID_2": {ID: 2, Name: "Bob", UID: "TEST_UID_2", DiscordID: "222222222"},
	}

	// Reset Active Attendees
	currentAttendees = make(map[string]time.Time)

	// Reset scan history
	scanHistory = nil

	// Reset Database (Use in-memory DB for speed)
	if db != nil {
		db.Close()
	}

	var err error
	db, err = sql.Open("sqlite", ":memory:")
	if err != nil {
		panic(err)
	}

	// Set WAL journal mode
	if _, err := db.Exec(`PRAGMA journal_mode = WAL;`); err != nil {
		panic(err)
	}
	if _, err := db.Exec(`PRAGMA busy_timeout = 5000;`); err != nil {
		panic(err)
	}

	// Enable foreign keys
	if _, err := db.Exec(`PRAGMA foreign_keys = ON;`); err != nil {
		panic(err)
	}

	// Create tables
	createMembersSQL := `CREATE TABLE IF NOT EXISTS members (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		uid TEXT NOT NULL UNIQUE,
		discord_id TEXT NOT NULL
	);`
	if _, err := db.Exec(createMembersSQL); err != nil {
		panic(err)
	}

	// Seed members according to userDB
	for _, m := range userDB {
		if _, err := db.Exec(`INSERT INTO members (id, name, uid, discord_id) VALUES (?, ?, ?, ?)`, m.ID, m.Name, m.UID, m.DiscordID); err != nil {
			panic(err)
		}
	}

	createSessionsSQL := `CREATE TABLE IF NOT EXISTS sessions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		member_id INTEGER NOT NULL,
		signin_time TEXT NOT NULL,
		signout_time TEXT NOT NULL,
		FOREIGN KEY(member_id) REFERENCES members(id) ON DELETE CASCADE
	);`
	if _, err := db.Exec(createSessionsSQL); err != nil {
		panic(err)
	}
}

func TestHandleScan_MethodNotAllowed(t *testing.T) {
	setupTest()

	req, _ := http.NewRequest("GET", "/scan", nil)
	rr := httptest.NewRecorder()

	handleScan(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 Method Not Allowed, got %v", rr.Code)
	}
}

func TestHandleScan_InvalidJSON(t *testing.T) {
	setupTest()

	req, _ := http.NewRequest("POST", "/scan", bytes.NewBuffer([]byte("{invalid")))
	rr := httptest.NewRecorder()

	handleScan(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request, got %v", rr.Code)
	}
}

func TestHandleScan_Login(t *testing.T) {
	setupTest()

	// Create a request (Alice taps card)
	payload := []byte(`{"uid": "TEST_UID_1"}`)
	req, _ := http.NewRequest("POST", "/scan", bytes.NewBuffer(payload))
	rr := httptest.NewRecorder()

	// Run handler
	handleScan(rr, req)

	// Check Response Code
	if status := rr.Code; status != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusOK)
	}

	// Check Response Body
	var resp map[string]string
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp["status"] != "in" {
		t.Errorf("expected status 'in', got %v", resp["status"])
	}

	// Verify Internal State
	if _, inside := currentAttendees["TEST_UID_1"]; !inside {
		t.Error("Alice should be in currentAttendees map")
	}
}

func TestHandleScan_Logout(t *testing.T) {
	setupTest()

	// Pre-condition: Alice is already inside
	currentAttendees["TEST_UID_1"] = time.Now().Add(-1 * time.Hour) // Entered 1 hour ago

	// Alice taps again
	payload := []byte(`{"uid": "TEST_UID_1"}`)
	req, _ := http.NewRequest("POST", "/scan", bytes.NewBuffer(payload))
	rr := httptest.NewRecorder()

	handleScan(rr, req)

	// Verify she is now OUT
	var resp map[string]string
	json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp["status"] != "out" {
		t.Errorf("expected status 'out', got %v", resp["status"])
	}

	// Verify she was removed from memory
	if _, inside := currentAttendees["TEST_UID_1"]; inside {
		t.Error("Alice should have been removed from currentAttendees")
	}

	// Verify DB record exists by joining sessions->members
	rows, _ := db.Query(`SELECT m.uid FROM sessions s JOIN members m ON m.id = s.member_id WHERE m.uid = 'TEST_UID_1'`)
	if !rows.Next() {
		t.Error("No session record found in SQLite")
	}
	rows.Close()
}

func TestHandleScan_UnknownUser(t *testing.T) {
	setupTest()

	payload := []byte(`{"uid": "UNKNOWN_UID"}`)
	req, _ := http.NewRequest("POST", "/scan", bytes.NewBuffer(payload))
	rr := httptest.NewRecorder()

	handleScan(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("Expected 403 Forbidden for unknown user, got %v", rr.Code)
	}
}

func TestHandleMembers_Create(t *testing.T) {
	setupTest()

	payload := []byte(`{"name":"Charlie","uid":"TEST_UID_3","discord_id":"333333333"}`)
	req, _ := http.NewRequest("POST", "/members", bytes.NewBuffer(payload))
	rr := httptest.NewRecorder()

	handleMembers(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected status 201 Created, got %v; body=%s", rr.Code, rr.Body.String())
	}

	var m Member
	if err := json.Unmarshal(rr.Body.Bytes(), &m); err != nil {
		t.Fatalf("failed to parse response JSON: %v", err)
	}
	if m.Name != "Charlie" || m.UID != "TEST_UID_3" || m.DiscordID != "333333333" {
		t.Fatalf("unexpected member returned: %+v", m)
	}

	// Verify DB has the member
	row := db.QueryRow(`SELECT name, uid, discord_id FROM members WHERE uid = ?`, "TEST_UID_3")
	var name, uid, discordID string
	if err := row.Scan(&name, &uid, &discordID); err != nil {
		t.Fatalf("member not found in DB: %v", err)
	}
	if discordID != "333333333" {
		t.Fatalf("expected discord_id '333333333', got %q", discordID)
	}
}

func TestHandleMembers_CreateDuplicate(t *testing.T) {
	setupTest()

	// First create
	payload := []byte(`{"name":"Dave","uid":"TEST_UID_4","discord_id":"444444444"}`)
	req1, _ := http.NewRequest("POST", "/members", bytes.NewBuffer(payload))
	rr1 := httptest.NewRecorder()
	handleMembers(rr1, req1)
	if rr1.Code != http.StatusCreated {
		t.Fatalf("first create failed: %v", rr1.Code)
	}

	// Second create with same UID -> should be Conflict
	req2, _ := http.NewRequest("POST", "/members", bytes.NewBuffer(payload))
	rr2 := httptest.NewRecorder()
	handleMembers(rr2, req2)
	if rr2.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict for duplicate uid, got %v", rr2.Code)
	}
}

func TestHandleMembers_InvalidJSON(t *testing.T) {
	setupTest()

	req, _ := http.NewRequest("POST", "/members", bytes.NewBuffer([]byte("{invalid")))
	rr := httptest.NewRecorder()

	handleMembers(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request, got %v", rr.Code)
	}
}

func TestHandleMembers_MethodNotAllowed(t *testing.T) {
	setupTest()

	req, _ := http.NewRequest("PUT", "/members", nil)
	rr := httptest.NewRecorder()

	handleMembers(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 Method Not Allowed, got %v", rr.Code)
	}
}

func TestHandleMembers_CreateMissingDiscordID(t *testing.T) {
	setupTest()

	// Try to create without discord_id
	payload := []byte(`{"name":"Frank","uid":"TEST_UID_6"}`)
	req, _ := http.NewRequest("POST", "/members", bytes.NewBuffer(payload))
	rr := httptest.NewRecorder()

	handleMembers(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for missing discord_id, got %v", rr.Code)
	}
}

func TestHandleMembers_CreateEmptyDiscordID(t *testing.T) {
	setupTest()

	// Try to create with empty discord_id
	payload := []byte(`{"name":"George","uid":"TEST_UID_7","discord_id":""}`)
	req, _ := http.NewRequest("POST", "/members", bytes.NewBuffer(payload))
	rr := httptest.NewRecorder()

	handleMembers(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for empty discord_id, got %v", rr.Code)
	}
}

func TestHandleMembers_List(t *testing.T) {
	setupTest()

	req, _ := http.NewRequest("GET", "/members", nil)
	rr := httptest.NewRecorder()
	handleMembers(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %v", rr.Code)
	}

	var members []Member
	if err := json.Unmarshal(rr.Body.Bytes(), &members); err != nil {
		t.Fatalf("failed to parse members list: %v", err)
	}
	if len(members) < 2 {
		t.Fatalf("expected at least 2 seeded members, got %d", len(members))
	}
}

func TestHandleCount_NoAttendees(t *testing.T) {
	setupTest()

	req, _ := http.NewRequest("GET", "/count", nil)
	rr := httptest.NewRecorder()

	handleCount(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %v", rr.Code)
	}

	var resp map[string]int
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	if resp["count"] != 0 {
		t.Errorf("expected count 0, got %d", resp["count"])
	}
}

func TestHandleCount_WithAttendees(t *testing.T) {
	setupTest()

	// Add two attendees
	currentAttendees["TEST_UID_1"] = time.Now()
	currentAttendees["TEST_UID_2"] = time.Now()

	req, _ := http.NewRequest("GET", "/count", nil)
	rr := httptest.NewRecorder()

	handleCount(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %v", rr.Code)
	}

	var resp map[string]int
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	if resp["count"] != 2 {
		t.Errorf("expected count 2, got %d", resp["count"])
	}
}

func TestHandleCurrent_Empty(t *testing.T) {
	setupTest()

	req, _ := http.NewRequest("GET", "/current", nil)
	rr := httptest.NewRecorder()

	handleCurrent(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %v", rr.Code)
	}

	var list []ActiveAttendee
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("expected empty list, got %d", len(list))
	}
}

func TestHandleCurrent_WithAttendees(t *testing.T) {
	setupTest()

	// Add attendees
	currentAttendees["TEST_UID_1"] = time.Now().Add(-10 * time.Minute)
	currentAttendees["TEST_UID_2"] = time.Now().Add(-5 * time.Minute)

	req, _ := http.NewRequest("GET", "/current", nil)
	rr := httptest.NewRecorder()

	handleCurrent(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %v", rr.Code)
	}

	var list []ActiveAttendee
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 attendees, got %d", len(list))
	}
	// Names should match userDB mapping
	names := map[string]bool{"Alice": false, "Bob": false}
	for _, a := range list {
		if _, ok := names[a.Name]; ok {
			names[a.Name] = true
		}
	}
	if !names["Alice"] || !names["Bob"] {
		t.Fatalf("expected both Alice and Bob present; got %+v", list)
	}
}

func TestHandleHealth(t *testing.T) {
	setupTest()

	req, _ := http.NewRequest("GET", "/health", nil)
	rr := httptest.NewRecorder()

	handleHealth(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 OK, got %v", rr.Code)
	}

	expected := "OK"
	if rr.Body.String() != expected {
		t.Errorf("expected body %q, got %q", expected, rr.Body.String())
	}
}

func TestHandleHistory_Empty(t *testing.T) {
	setupTest()

	req, _ := http.NewRequest("GET", "/history", nil)
	rr := httptest.NewRecorder()

	handleHistory(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %v", rr.Code)
	}

	var sessions []Session
	if err := json.Unmarshal(rr.Body.Bytes(), &sessions); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("expected 0 sessions, got %d", len(sessions))
	}
}

func TestHandleHistory_WithSessions(t *testing.T) {
	setupTest()

	// Insert sessions for Alice and Bob
	now := time.Now()
	_, err := db.Exec(`INSERT INTO sessions (member_id, signin_time, signout_time) VALUES (?, ?, ?)`, 1, now.Add(-2*time.Hour).Format(time.RFC3339), now.Add(-1*time.Hour).Format(time.RFC3339))
	if err != nil {
		t.Fatalf("failed inserting session: %v", err)
	}
	_, err = db.Exec(`INSERT INTO sessions (member_id, signin_time, signout_time) VALUES (?, ?, ?)`, 2, now.Add(-3*time.Hour).Format(time.RFC3339), now.Add(-2*time.Hour+30*time.Minute).Format(time.RFC3339))
	if err != nil {
		t.Fatalf("failed inserting session: %v", err)
	}

	req, _ := http.NewRequest("GET", "/history", nil)
	rr := httptest.NewRecorder()

	handleHistory(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %v", rr.Code)
	}

	var sessions []Session
	if err := json.Unmarshal(rr.Body.Bytes(), &sessions); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(sessions))
	}
	// Ensure names exist
	found := map[string]bool{"Alice": false, "Bob": false}
	for _, s := range sessions {
		if _, ok := found[s.Name]; ok {
			found[s.Name] = true
		}
	}
	if !found["Alice"] || !found["Bob"] {
		t.Fatalf("expected sessions for Alice and Bob, got %+v", sessions)
	}
}

func TestHandleHistory_WithFromFilter(t *testing.T) {
	setupTest()

	// Insert 3 sessions at different times
	baseTime := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	_, err := db.Exec(`INSERT INTO sessions (member_id, signin_time, signout_time) VALUES (?, ?, ?)`,
		1, baseTime.Add(-2*time.Hour).Format(time.RFC3339), baseTime.Add(-1*time.Hour).Format(time.RFC3339))
	if err != nil {
		t.Fatalf("failed inserting session: %v", err)
	}
	_, err = db.Exec(`INSERT INTO sessions (member_id, signin_time, signout_time) VALUES (?, ?, ?)`,
		2, baseTime.Format(time.RFC3339), baseTime.Add(1*time.Hour).Format(time.RFC3339))
	if err != nil {
		t.Fatalf("failed inserting session: %v", err)
	}
	_, err = db.Exec(`INSERT INTO sessions (member_id, signin_time, signout_time) VALUES (?, ?, ?)`,
		1, baseTime.Add(2*time.Hour).Format(time.RFC3339), baseTime.Add(3*time.Hour).Format(time.RFC3339))
	if err != nil {
		t.Fatalf("failed inserting session: %v", err)
	}

	// Filter to get only sessions from baseTime onwards
	req, _ := http.NewRequest("GET", "/history?from="+baseTime.Format(time.RFC3339), nil)
	rr := httptest.NewRecorder()

	handleHistory(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %v", rr.Code)
	}

	var sessions []Session
	if err := json.Unmarshal(rr.Body.Bytes(), &sessions); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("expected 2 sessions with from filter, got %d", len(sessions))
	}
}

func TestHandleHistory_WithToFilter(t *testing.T) {
	setupTest()

	// Insert 3 sessions at different times
	baseTime := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	_, err := db.Exec(`INSERT INTO sessions (member_id, signin_time, signout_time) VALUES (?, ?, ?)`,
		1, baseTime.Add(-2*time.Hour).Format(time.RFC3339), baseTime.Add(-1*time.Hour).Format(time.RFC3339))
	if err != nil {
		t.Fatalf("failed inserting session: %v", err)
	}
	_, err = db.Exec(`INSERT INTO sessions (member_id, signin_time, signout_time) VALUES (?, ?, ?)`,
		2, baseTime.Format(time.RFC3339), baseTime.Add(1*time.Hour).Format(time.RFC3339))
	if err != nil {
		t.Fatalf("failed inserting session: %v", err)
	}
	_, err = db.Exec(`INSERT INTO sessions (member_id, signin_time, signout_time) VALUES (?, ?, ?)`,
		1, baseTime.Add(2*time.Hour).Format(time.RFC3339), baseTime.Add(3*time.Hour).Format(time.RFC3339))
	if err != nil {
		t.Fatalf("failed inserting session: %v", err)
	}

	// Filter to get only sessions up to baseTime
	req, _ := http.NewRequest("GET", "/history?to="+baseTime.Format(time.RFC3339), nil)
	rr := httptest.NewRecorder()

	handleHistory(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %v", rr.Code)
	}

	var sessions []Session
	if err := json.Unmarshal(rr.Body.Bytes(), &sessions); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("expected 2 sessions with to filter, got %d", len(sessions))
	}
}

func TestHandleHistory_WithFromAndToFilter(t *testing.T) {
	setupTest()

	// Insert 5 sessions at different times
	baseTime := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	sessions := []struct {
		memberID int
		offset   time.Duration
	}{
		{1, -4 * time.Hour},
		{2, -2 * time.Hour},
		{1, 0},
		{2, 2 * time.Hour},
		{1, 4 * time.Hour},
	}

	for _, s := range sessions {
		signinTime := baseTime.Add(s.offset)
		signoutTime := signinTime.Add(30 * time.Minute)
		_, err := db.Exec(`INSERT INTO sessions (member_id, signin_time, signout_time) VALUES (?, ?, ?)`,
			s.memberID, signinTime.Format(time.RFC3339), signoutTime.Format(time.RFC3339))
		if err != nil {
			t.Fatalf("failed inserting session: %v", err)
		}
	}

	// Filter to get only sessions between -2 hours and +2 hours
	from := baseTime.Add(-2 * time.Hour).Format(time.RFC3339)
	to := baseTime.Add(2 * time.Hour).Format(time.RFC3339)
	req, _ := http.NewRequest("GET", "/history?from="+from+"&to="+to, nil)
	rr := httptest.NewRecorder()

	handleHistory(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %v", rr.Code)
	}

	var result []Session
	if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if len(result) != 3 {
		t.Fatalf("expected 3 sessions with from and to filter, got %d", len(result))
	}
}

func TestHandleHistory_WithLimit(t *testing.T) {
	setupTest()

	// Insert 5 sessions
	baseTime := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		signinTime := baseTime.Add(time.Duration(i) * time.Hour)
		signoutTime := signinTime.Add(30 * time.Minute)
		memberID := (i % 2) + 1 // Alternate between member 1 and 2
		_, err := db.Exec(`INSERT INTO sessions (member_id, signin_time, signout_time) VALUES (?, ?, ?)`,
			memberID, signinTime.Format(time.RFC3339), signoutTime.Format(time.RFC3339))
		if err != nil {
			t.Fatalf("failed inserting session: %v", err)
		}
	}

	// Request only 3 most recent sessions
	req, _ := http.NewRequest("GET", "/history?limit=3", nil)
	rr := httptest.NewRecorder()

	handleHistory(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %v", rr.Code)
	}

	var sessions []Session
	if err := json.Unmarshal(rr.Body.Bytes(), &sessions); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if len(sessions) != 3 {
		t.Fatalf("expected 3 sessions with limit, got %d", len(sessions))
	}

	// Verify they are the newest (should be in descending order)
	for i := 0; i < len(sessions)-1; i++ {
		if sessions[i].SignInTime.Before(sessions[i+1].SignInTime) {
			t.Fatalf("sessions not in descending order")
		}
	}
}

func TestHandleHistory_WithAllFilters(t *testing.T) {
	setupTest()

	// Insert 10 sessions
	baseTime := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		signinTime := baseTime.Add(time.Duration(i) * time.Hour)
		signoutTime := signinTime.Add(30 * time.Minute)
		memberID := (i % 2) + 1
		_, err := db.Exec(`INSERT INTO sessions (member_id, signin_time, signout_time) VALUES (?, ?, ?)`,
			memberID, signinTime.Format(time.RFC3339), signoutTime.Format(time.RFC3339))
		if err != nil {
			t.Fatalf("failed inserting session: %v", err)
		}
	}

	// Request with from, to, and limit
	from := baseTime.Add(2 * time.Hour).Format(time.RFC3339)
	to := baseTime.Add(8 * time.Hour).Format(time.RFC3339)
	req, _ := http.NewRequest("GET", "/history?from="+from+"&to="+to+"&limit=3", nil)
	rr := httptest.NewRecorder()

	handleHistory(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %v", rr.Code)
	}

	var sessions []Session
	if err := json.Unmarshal(rr.Body.Bytes(), &sessions); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	// Should have 7 sessions in range (hours 2-8 inclusive), but limited to 3
	if len(sessions) != 3 {
		t.Fatalf("expected 3 sessions with all filters, got %d", len(sessions))
	}
}

func TestHandleHistory_InvalidFromDate(t *testing.T) {
	setupTest()

	req, _ := http.NewRequest("GET", "/history?from=invalid-date", nil)
	rr := httptest.NewRecorder()

	handleHistory(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for invalid from date, got %v", rr.Code)
	}
}

func TestHandleHistory_InvalidToDate(t *testing.T) {
	setupTest()

	req, _ := http.NewRequest("GET", "/history?to=invalid-date", nil)
	rr := httptest.NewRecorder()

	handleHistory(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for invalid to date, got %v", rr.Code)
	}
}

func TestHandleHistory_InvalidLimit(t *testing.T) {
	setupTest()

	req, _ := http.NewRequest("GET", "/history?limit=abc", nil)
	rr := httptest.NewRecorder()

	handleHistory(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for invalid limit, got %v", rr.Code)
	}
}

func TestHandleHistory_NegativeLimit(t *testing.T) {
	setupTest()

	req, _ := http.NewRequest("GET", "/history?limit=-5", nil)
	rr := httptest.NewRecorder()

	handleHistory(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for negative limit, got %v", rr.Code)
	}
}

func TestHandleScanHistory_Empty(t *testing.T) {
	setupTest()

	req, _ := http.NewRequest("GET", "/scan_history", nil)
	rr := httptest.NewRecorder()

	handleScanHistory(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %v", rr.Code)
	}

	var events []ScanEvent
	if err := json.Unmarshal(rr.Body.Bytes(), &events); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected 0 scan events, got %d", len(events))
	}
}

func TestHandleScanHistory_FromHandleScan(t *testing.T) {
	setupTest()

	// Two scans in chronological order
	firstPayload := []byte(`{"uid": "TEST_UID_1"}`)
	secondPayload := []byte(`{"uid": "TEST_UID_2"}`)

	req1, _ := http.NewRequest("POST", "/scan", bytes.NewBuffer(firstPayload))
	rr1 := httptest.NewRecorder()
	handleScan(rr1, req1)

	req2, _ := http.NewRequest("POST", "/scan", bytes.NewBuffer(secondPayload))
	rr2 := httptest.NewRecorder()
	handleScan(rr2, req2)

	reqHistory, _ := http.NewRequest("GET", "/scan_history", nil)
	rrHistory := httptest.NewRecorder()
	handleScanHistory(rrHistory, reqHistory)

	if rrHistory.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %v", rrHistory.Code)
	}

	var events []ScanEvent
	if err := json.Unmarshal(rrHistory.Body.Bytes(), &events); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	if len(events) != 2 {
		t.Fatalf("expected 2 scan events, got %d", len(events))
	}

	// Newest first
	if events[0].UID != "TEST_UID_2" || events[1].UID != "TEST_UID_1" {
		t.Fatalf("unexpected order of scan events: %+v", events)
	}

	if events[0].Time.Before(events[1].Time) {
		t.Fatalf("expected newest event first, got timestamps %v then %v", events[0].Time, events[1].Time)
	}
}

func TestHandleScanHistory_TrimsToTen(t *testing.T) {
	setupTest()

	start := time.Now().Add(-20 * time.Minute)
	for i := 0; i < 12; i++ {
		uid := fmt.Sprintf("UID_%d", i)
		recordScanEvent(uid, start.Add(time.Duration(i)*time.Minute))
	}

	req, _ := http.NewRequest("GET", "/scan_history", nil)
	rr := httptest.NewRecorder()
	handleScanHistory(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %v", rr.Code)
	}

	var events []ScanEvent
	if err := json.Unmarshal(rr.Body.Bytes(), &events); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	if len(events) != 10 {
		t.Fatalf("expected 10 events, got %d", len(events))
	}

	if events[0].UID != "UID_11" || events[9].UID != "UID_2" {
		t.Fatalf("unexpected event range/order: %+v", events)
	}

	if events[0].Time.Before(events[1].Time) {
		t.Fatalf("expected newest first, got %v then %v", events[0].Time, events[1].Time)
	}
}

func TestHandleSignoutAll_NoAttendees(t *testing.T) {
	setupTest()

	req, _ := http.NewRequest("POST", "/signout_all", nil)
	rr := httptest.NewRecorder()

	handleSignoutAll(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %v", rr.Code)
	}

	var resp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	if !bytes.Contains([]byte(resp["message"]), []byte("0 total")) {
		t.Errorf("expected message to contain '0 total', got %q", resp["message"])
	}

	// Verify attendees map is empty
	if len(currentAttendees) != 0 {
		t.Errorf("expected currentAttendees to be empty, got %d", len(currentAttendees))
	}
}

func TestHandleSignoutAll_WithAttendees(t *testing.T) {
	setupTest()

	// Add three attendees
	currentAttendees["TEST_UID_1"] = time.Now().Add(-30 * time.Minute)
	currentAttendees["TEST_UID_2"] = time.Now().Add(-20 * time.Minute)

	req, _ := http.NewRequest("POST", "/signout_all", nil)
	rr := httptest.NewRecorder()

	handleSignoutAll(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %v", rr.Code)
	}

	var resp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	if !bytes.Contains([]byte(resp["message"]), []byte("2 total")) {
		t.Errorf("expected message to contain '2 total', got %q", resp["message"])
	}

	// Verify attendees map is cleared
	if len(currentAttendees) != 0 {
		t.Errorf("expected currentAttendees to be empty, got %d", len(currentAttendees))
	}
}

func TestHandleSignoutAll_MethodNotAllowed(t *testing.T) {
	setupTest()

	// Try with GET instead of POST
	req, _ := http.NewRequest("GET", "/signout_all", nil)
	rr := httptest.NewRecorder()

	handleSignoutAll(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 Method Not Allowed, got %v", rr.Code)
	}
}

func TestHandleExportMembers_Success(t *testing.T) {
	setupTest()

	// Backup existing file if present
	orig, err := os.ReadFile(membersFilePath)
	hasOrig := err == nil
	if hasOrig {
		defer os.WriteFile(membersFilePath, orig, 0644)
	} else {
		defer os.Remove(membersFilePath)
	}

	req, _ := http.NewRequest("GET", "/export_members", nil)
	rr := httptest.NewRecorder()
	handleExportMembers(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %v; body=%s", rr.Code, rr.Body.String())
	}

	// Verify file written and parse it
	data, err := os.ReadFile(membersFilePath)
	if err != nil {
		t.Fatalf("expected members file to be written: %v", err)
	}
	var members []Member
	if err := json.Unmarshal(data, &members); err != nil {
		t.Fatalf("failed to parse exported members: %v", err)
	}
	if len(members) < 2 {
		t.Fatalf("expected at least 2 members exported, got %d", len(members))
	}
}

func TestHandleImportMembers_Success(t *testing.T) {
	setupTest()

	// Backup existing file if present
	orig, err := os.ReadFile(membersFilePath)
	hasOrig := err == nil
	if hasOrig {
		defer os.WriteFile(membersFilePath, orig, 0644)
	} else {
		defer os.Remove(membersFilePath)
	}

	// Prepare import file with a new member
	toImport := []Member{{Name: "Zara", UID: "TEST_UID_Z", DiscordID: "999000"}}
	data, _ := json.Marshal(toImport)
	if err := os.WriteFile(membersFilePath, data, 0644); err != nil {
		t.Fatalf("failed to write import file: %v", err)
	}

	req, _ := http.NewRequest("POST", "/import_members", nil)
	rr := httptest.NewRecorder()
	handleImportMembers(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %v; body=%s", rr.Code, rr.Body.String())
	}

	// Verify member exists in DB
	row := db.QueryRow(`SELECT name, uid, discord_id FROM members WHERE uid = ?`, "TEST_UID_Z")
	var name, uid, discordID string
	if err := row.Scan(&name, &uid, &discordID); err != nil {
		t.Fatalf("imported member not found in DB: %v", err)
	}
	if name != "Zara" || discordID != "999000" {
		t.Fatalf("unexpected member data: %s %s %s", name, uid, discordID)
	}
}

func TestHandleImportMembers_MethodNotAllowed(t *testing.T) {
	setupTest()

	req, _ := http.NewRequest("GET", "/import_members", nil)
	rr := httptest.NewRecorder()
	handleImportMembers(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 Method Not Allowed, got %v", rr.Code)
	}
}

func TestHandleSignInWithDiscordID_Success(t *testing.T) {
	setupTest()

	payload := []byte(`{"discord_id":"111111111"}`)
	req, _ := http.NewRequest("POST", "/signin_discord", bytes.NewBuffer(payload))
	rr := httptest.NewRecorder()

	handleSignInWithDiscordID(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %v; body=%s", rr.Code, rr.Body.String())
	}

	var resp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	if resp["status"] != "in" {
		t.Errorf("expected status 'in', got %v", resp["status"])
	}

	// Verify member is in currentAttendees
	if _, inside := currentAttendees["TEST_UID_1"]; !inside {
		t.Error("Alice should be in currentAttendees map")
	}
}

func TestHandleSignInWithDiscordID_MemberNotFound(t *testing.T) {
	setupTest()

	payload := []byte(`{"discord_id":"999999999"}`)
	req, _ := http.NewRequest("POST", "/signin_discord", bytes.NewBuffer(payload))
	rr := httptest.NewRecorder()

	handleSignInWithDiscordID(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404 Not Found, got %v", rr.Code)
	}
}

func TestHandleSignInWithDiscordID_AlreadySignedIn(t *testing.T) {
	setupTest()

	// Pre-condition: Alice is already signed in
	currentAttendees["TEST_UID_1"] = time.Now()

	payload := []byte(`{"discord_id":"111111111"}`)
	req, _ := http.NewRequest("POST", "/signin_discord", bytes.NewBuffer(payload))
	rr := httptest.NewRecorder()

	handleSignInWithDiscordID(rr, req)

	if rr.Code != http.StatusConflict {
		t.Errorf("expected 409 Conflict, got %v", rr.Code)
	}
}

func TestHandleSignInWithDiscordID_InvalidJSON(t *testing.T) {
	setupTest()

	payload := []byte(`{invalid json}`)
	req, _ := http.NewRequest("POST", "/signin_discord", bytes.NewBuffer(payload))
	rr := httptest.NewRecorder()

	handleSignInWithDiscordID(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request, got %v", rr.Code)
	}
}

func TestHandleSignInWithDiscordID_MethodNotAllowed(t *testing.T) {
	setupTest()

	req, _ := http.NewRequest("GET", "/signin_discord", nil)
	rr := httptest.NewRecorder()

	handleSignInWithDiscordID(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 Method Not Allowed, got %v", rr.Code)
	}
}

func TestHandleSignOutWithDiscordID_Success(t *testing.T) {
	setupTest()

	// Pre-condition: Alice is already signed in
	currentAttendees["TEST_UID_1"] = time.Now().Add(-1 * time.Hour)

	payload := []byte(`{"discord_id":"111111111"}`)
	req, _ := http.NewRequest("POST", "/signout_discord", bytes.NewBuffer(payload))
	rr := httptest.NewRecorder()

	handleSignOutWithDiscordID(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %v; body=%s", rr.Code, rr.Body.String())
	}

	var resp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	if resp["status"] != "out" {
		t.Errorf("expected status 'out', got %v", resp["status"])
	}

	// Verify member is removed from currentAttendees
	if _, inside := currentAttendees["TEST_UID_1"]; inside {
		t.Error("Alice should be removed from currentAttendees map")
	}

	// Verify session was saved to DB
	rows, _ := db.Query(`SELECT m.uid FROM sessions s JOIN members m ON m.id = s.member_id WHERE m.uid = 'TEST_UID_1'`)
	if !rows.Next() {
		t.Error("No session record found in database")
	}
	rows.Close()
}

func TestHandleSignOutWithDiscordID_MemberNotFound(t *testing.T) {
	setupTest()

	payload := []byte(`{"discord_id":"999999999"}`)
	req, _ := http.NewRequest("POST", "/signout_discord", bytes.NewBuffer(payload))
	rr := httptest.NewRecorder()

	handleSignOutWithDiscordID(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404 Not Found, got %v", rr.Code)
	}
}

func TestHandleSignOutWithDiscordID_NotSignedIn(t *testing.T) {
	setupTest()

	// Alice is NOT signed in
	payload := []byte(`{"discord_id":"111111111"}`)
	req, _ := http.NewRequest("POST", "/signout_discord", bytes.NewBuffer(payload))
	rr := httptest.NewRecorder()

	handleSignOutWithDiscordID(rr, req)

	if rr.Code != http.StatusConflict {
		t.Errorf("expected 409 Conflict, got %v", rr.Code)
	}
}

func TestHandleSignOutWithDiscordID_InvalidJSON(t *testing.T) {
	setupTest()

	payload := []byte(`{invalid json}`)
	req, _ := http.NewRequest("POST", "/signout_discord", bytes.NewBuffer(payload))
	rr := httptest.NewRecorder()

	handleSignOutWithDiscordID(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request, got %v", rr.Code)
	}
}

func TestHandleSignOutWithDiscordID_MethodNotAllowed(t *testing.T) {
	setupTest()

	req, _ := http.NewRequest("GET", "/signout_discord", nil)
	rr := httptest.NewRecorder()

	handleSignOutWithDiscordID(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405 Method Not Allowed, got %v", rr.Code)
	}
}

func TestAPIKeyMiddleware_NoKeysConfigured(t *testing.T) {
	setupTest()

	// Reset API keys to empty (no authentication required)
	validAPIKeys = map[string]bool{}

	// Create a test handler
	testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("success"))
	})

	// Wrap with middleware
	wrappedHandler := apiKeyMiddleware(testHandler)

	// Test request without API key - should succeed when no keys configured
	req, _ := http.NewRequest("GET", "/test", nil)
	rr := httptest.NewRecorder()

	wrappedHandler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 OK when no keys configured, got %v", rr.Code)
	}
	if rr.Body.String() != "success" {
		t.Errorf("expected 'success' body, got %v", rr.Body.String())
	}
}

func TestAPIKeyMiddleware_ValidKey(t *testing.T) {
	setupTest()

	// Configure API keys
	validAPIKeys = map[string]bool{
		"test-key-123": true,
		"test-key-456": true,
	}

	// Create a test handler
	testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("authenticated"))
	})

	// Wrap with middleware
	wrappedHandler := apiKeyMiddleware(testHandler)

	// Test request with valid API key
	req, _ := http.NewRequest("GET", "/test", nil)
	req.Header.Set("X-API-Key", "test-key-123")
	rr := httptest.NewRecorder()

	wrappedHandler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 OK with valid key, got %v", rr.Code)
	}
	if rr.Body.String() != "authenticated" {
		t.Errorf("expected 'authenticated' body, got %v", rr.Body.String())
	}
}

func TestAPIKeyMiddleware_InvalidKey(t *testing.T) {
	setupTest()

	// Configure API keys
	validAPIKeys = map[string]bool{
		"valid-key": true,
	}

	// Create a test handler
	testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("should not reach here"))
	})

	// Wrap with middleware
	wrappedHandler := apiKeyMiddleware(testHandler)

	// Test request with invalid API key
	req, _ := http.NewRequest("GET", "/test", nil)
	req.Header.Set("X-API-Key", "invalid-key")
	rr := httptest.NewRecorder()

	wrappedHandler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 Unauthorized with invalid key, got %v", rr.Code)
	}

	// Check response body
	var response map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatalf("failed to parse error response: %v", err)
	}
	if response["error"] != "missing or invalid API key" {
		t.Errorf("expected error message about invalid key, got %v", response["error"])
	}
}

func TestAPIKeyMiddleware_MissingKey(t *testing.T) {
	setupTest()

	// Configure API keys
	validAPIKeys = map[string]bool{
		"required-key": true,
	}

	// Create a test handler
	testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("should not reach here"))
	})

	// Wrap with middleware
	wrappedHandler := apiKeyMiddleware(testHandler)

	// Test request without API key header
	req, _ := http.NewRequest("POST", "/scan", nil)
	rr := httptest.NewRecorder()

	wrappedHandler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 Unauthorized when key is missing, got %v", rr.Code)
	}

	// Check response body
	var response map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatalf("failed to parse error response: %v", err)
	}
	if response["error"] != "missing or invalid API key" {
		t.Errorf("expected error message about missing key, got %v", response["error"])
	}
}

func TestAPIKeyMiddleware_HealthEndpointAlwaysAllowed(t *testing.T) {
	setupTest()

	// Configure API keys
	validAPIKeys = map[string]bool{
		"some-key": true,
	}

	// Create a test handler (simulating health endpoint)
	testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	// Wrap with middleware
	wrappedHandler := apiKeyMiddleware(testHandler)

	// Test request to /health without API key - should always succeed
	req, _ := http.NewRequest("GET", "/health", nil)
	rr := httptest.NewRecorder()

	wrappedHandler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 OK for /health without key, got %v", rr.Code)
	}
	if rr.Body.String() != "OK" {
		t.Errorf("expected 'OK' body, got %v", rr.Body.String())
	}
}

func TestAPIKeyMiddleware_MultipleValidKeys(t *testing.T) {
	setupTest()

	// Configure multiple API keys
	validAPIKeys = map[string]bool{
		"scanner-key": true,
		"bot-key":     true,
		"admin-key":   true,
	}

	// Create a test handler
	testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("success"))
	})

	// Wrap with middleware
	wrappedHandler := apiKeyMiddleware(testHandler)

	// Test each valid key
	validKeys := []string{"scanner-key", "bot-key", "admin-key"}
	for _, key := range validKeys {
		req, _ := http.NewRequest("GET", "/test", nil)
		req.Header.Set("X-API-Key", key)
		rr := httptest.NewRecorder()

		wrappedHandler.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Errorf("expected 200 OK with key '%s', got %v", key, rr.Code)
		}
	}
}

func TestLoadAPIKeys_FromEnvironment(t *testing.T) {
	// Save original env vars
	originalScanner := os.Getenv("SCANNER_API_KEY")
	originalBot := os.Getenv("DISCORD_BOT_API_KEY")
	originalKeys := os.Getenv("API_KEYS")

	// Clean up after test
	defer func() {
		os.Setenv("SCANNER_API_KEY", originalScanner)
		os.Setenv("DISCORD_BOT_API_KEY", originalBot)
		os.Setenv("API_KEYS", originalKeys)
	}()

	// Test 1: Load from individual env vars
	os.Setenv("SCANNER_API_KEY", "scanner-123")
	os.Setenv("DISCORD_BOT_API_KEY", "bot-456")
	os.Setenv("API_KEYS", "")

	keys := loadAPIKeys()

	if len(keys) != 2 {
		t.Errorf("expected 2 keys, got %d", len(keys))
	}
	if !keys["scanner-123"] {
		t.Error("expected scanner-123 to be in keys")
	}
	if !keys["bot-456"] {
		t.Error("expected bot-456 to be in keys")
	}

	// Test 2: Load from comma-separated API_KEYS
	os.Setenv("SCANNER_API_KEY", "")
	os.Setenv("DISCORD_BOT_API_KEY", "")
	os.Setenv("API_KEYS", "key1,key2,key3")

	keys = loadAPIKeys()

	if len(keys) != 3 {
		t.Errorf("expected 3 keys, got %d", len(keys))
	}
	if !keys["key1"] || !keys["key2"] || !keys["key3"] {
		t.Error("expected all keys from API_KEYS to be loaded")
	}

	// Test 3: Load from both sources (should combine)
	os.Setenv("SCANNER_API_KEY", "scanner-key")
	os.Setenv("DISCORD_BOT_API_KEY", "bot-key")
	os.Setenv("API_KEYS", "admin-key,monitor-key")

	keys = loadAPIKeys()

	if len(keys) != 4 {
		t.Errorf("expected 4 keys, got %d", len(keys))
	}
	if !keys["scanner-key"] || !keys["bot-key"] || !keys["admin-key"] || !keys["monitor-key"] {
		t.Error("expected all keys from both sources to be loaded")
	}

	// Test 4: Handle whitespace in comma-separated list
	os.Setenv("SCANNER_API_KEY", "")
	os.Setenv("DISCORD_BOT_API_KEY", "")
	os.Setenv("API_KEYS", " key1 , key2 , key3 ")

	keys = loadAPIKeys()

	if len(keys) != 3 {
		t.Errorf("expected 3 keys (whitespace trimmed), got %d", len(keys))
	}
	if !keys["key1"] || !keys["key2"] || !keys["key3"] {
		t.Error("expected whitespace to be trimmed from keys")
	}

	// Test 5: Empty environment (no keys)
	os.Setenv("SCANNER_API_KEY", "")
	os.Setenv("DISCORD_BOT_API_KEY", "")
	os.Setenv("API_KEYS", "")

	keys = loadAPIKeys()

	if len(keys) != 0 {
		t.Errorf("expected 0 keys when env is empty, got %d", len(keys))
	}
}

func TestIntegration_ScanWithAPIKey(t *testing.T) {
	setupTest()

	// Configure API key
	validAPIKeys = map[string]bool{
		"scanner-key-xyz": true,
	}

	// Create a scan request
	payload := []byte(`{"uid":"TEST_UID_1"}`)
	req, _ := http.NewRequest("POST", "/scan", bytes.NewBuffer(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "scanner-key-xyz")
	rr := httptest.NewRecorder()

	// Wrap handler with middleware
	wrappedHandler := apiKeyMiddleware(handleScan)
	wrappedHandler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 OK, got %v", rr.Code)
	}

	// Verify scan worked
	var response map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if response["status"] != "in" {
		t.Errorf("expected status 'in', got %v", response["status"])
	}
}

func TestIntegration_ScanWithoutAPIKeyWhenRequired(t *testing.T) {
	setupTest()

	// Configure API key (making it required)
	validAPIKeys = map[string]bool{
		"required-key": true,
	}

	// Create a scan request WITHOUT API key
	payload := []byte(`{"uid":"TEST_UID_1"}`)
	req, _ := http.NewRequest("POST", "/scan", bytes.NewBuffer(payload))
	req.Header.Set("Content-Type", "application/json")
	// Note: No X-API-Key header
	rr := httptest.NewRecorder()

	// Wrap handler with middleware
	wrappedHandler := apiKeyMiddleware(handleScan)
	wrappedHandler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 Unauthorized, got %v", rr.Code)
	}

	// Verify error response
	var response map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatalf("failed to parse error response: %v", err)
	}
	if response["error"] != "missing or invalid API key" {
		t.Errorf("expected authentication error, got %v", response["error"])
	}
}

func TestHandleMember_UpdateSuccess(t *testing.T) {
	setupTest()

	// Update Alice's information
	payload := []byte(`{"name":"Alice Updated","uid":"TEST_UID_1","discord_id":"999999999"}`)
	req, _ := http.NewRequest("PUT", "/members/1", bytes.NewBuffer(payload))
	rr := httptest.NewRecorder()

	handleMember(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200 OK, got %v; body=%s", rr.Code, rr.Body.String())
	}

	var m Member
	if err := json.Unmarshal(rr.Body.Bytes(), &m); err != nil {
		t.Fatalf("failed to parse response JSON: %v", err)
	}
	if m.Name != "Alice Updated" || m.UID != "TEST_UID_1" || m.DiscordID != "999999999" {
		t.Fatalf("unexpected member returned: %+v", m)
	}

	// Verify DB has the updated member
	row := db.QueryRow(`SELECT name, uid, discord_id FROM members WHERE id = ?`, 1)
	var name, uid, discordID string
	if err := row.Scan(&name, &uid, &discordID); err != nil {
		t.Fatalf("member not found in DB: %v", err)
	}
	if name != "Alice Updated" || uid != "TEST_UID_1" || discordID != "999999999" {
		t.Fatalf("expected updated values, got name=%q uid=%q discord_id=%q", name, uid, discordID)
	}

	// Verify cache was reloaded
	if userDB["TEST_UID_1"].Name != "Alice Updated" {
		t.Fatalf("cache not updated, got name=%q", userDB["TEST_UID_1"].Name)
	}
}

func TestHandleMember_UpdateNotFound(t *testing.T) {
	setupTest()

	// Try to update non-existent member
	payload := []byte(`{"name":"Nobody","uid":"TEST_UID_999","discord_id":"000000000"}`)
	req, _ := http.NewRequest("PUT", "/members/999", bytes.NewBuffer(payload))
	rr := httptest.NewRecorder()

	handleMember(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected status 404 Not Found, got %v", rr.Code)
	}
}

func TestHandleMember_UpdateDuplicateUID(t *testing.T) {
	setupTest()

	// Try to update Alice to use Bob's UID
	payload := []byte(`{"name":"Alice","uid":"TEST_UID_2","discord_id":"111111111"}`)
	req, _ := http.NewRequest("PUT", "/members/1", bytes.NewBuffer(payload))
	rr := httptest.NewRecorder()

	handleMember(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("expected status 409 Conflict for duplicate UID, got %v", rr.Code)
	}
}

func TestHandleMember_UpdateInvalidJSON(t *testing.T) {
	setupTest()

	req, _ := http.NewRequest("PUT", "/members/1", bytes.NewBuffer([]byte("{invalid")))
	rr := httptest.NewRecorder()

	handleMember(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request, got %v", rr.Code)
	}
}

func TestHandleMember_UpdateMissingFields(t *testing.T) {
	setupTest()

	// Missing discord_id
	payload := []byte(`{"name":"Alice","uid":"TEST_UID_1"}`)
	req, _ := http.NewRequest("PUT", "/members/1", bytes.NewBuffer(payload))
	rr := httptest.NewRecorder()

	handleMember(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for missing discord_id, got %v", rr.Code)
	}
}

func TestHandleMember_UpdateEmptyFields(t *testing.T) {
	setupTest()

	// Empty name
	payload := []byte(`{"name":"","uid":"TEST_UID_1","discord_id":"111111111"}`)
	req, _ := http.NewRequest("PUT", "/members/1", bytes.NewBuffer(payload))
	rr := httptest.NewRecorder()

	handleMember(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for empty name, got %v", rr.Code)
	}
}

func TestHandleMember_UpdateInvalidID(t *testing.T) {
	setupTest()

	payload := []byte(`{"name":"Alice","uid":"TEST_UID_1","discord_id":"111111111"}`)
	req, _ := http.NewRequest("PUT", "/members/abc", bytes.NewBuffer(payload))
	rr := httptest.NewRecorder()

	handleMember(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for invalid ID, got %v", rr.Code)
	}
}

func TestHandleMember_UpdateNoID(t *testing.T) {
	setupTest()

	payload := []byte(`{"name":"Alice","uid":"TEST_UID_1","discord_id":"111111111"}`)
	req, _ := http.NewRequest("PUT", "/members/", bytes.NewBuffer(payload))
	rr := httptest.NewRecorder()

	handleMember(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for missing ID, got %v", rr.Code)
	}
}

func TestHandleMember_MethodNotAllowed(t *testing.T) {
	setupTest()

	req, _ := http.NewRequest("GET", "/members/1", nil)
	rr := httptest.NewRecorder()

	handleMember(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 Method Not Allowed, got %v", rr.Code)
	}
}

func TestHandleMember_UpdateWithWhitespace(t *testing.T) {
	setupTest()

	// Test that whitespace is trimmed
	payload := []byte(`{"name":"  Alice Trimmed  ","uid":" TEST_UID_1 ","discord_id":" 888888888 "}`)
	req, _ := http.NewRequest("PUT", "/members/1", bytes.NewBuffer(payload))
	rr := httptest.NewRecorder()

	handleMember(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200 OK, got %v; body=%s", rr.Code, rr.Body.String())
	}

	var m Member
	if err := json.Unmarshal(rr.Body.Bytes(), &m); err != nil {
		t.Fatalf("failed to parse response JSON: %v", err)
	}
	if m.Name != "Alice Trimmed" || m.UID != "TEST_UID_1" || m.DiscordID != "888888888" {
		t.Fatalf("whitespace not trimmed correctly: %+v", m)
	}

	// Verify DB has trimmed values
	row := db.QueryRow(`SELECT name, uid, discord_id FROM members WHERE id = ?`, 1)
	var name, uid, discordID string
	if err := row.Scan(&name, &uid, &discordID); err != nil {
		t.Fatalf("member not found in DB: %v", err)
	}
	if name != "Alice Trimmed" || uid != "TEST_UID_1" || discordID != "888888888" {
		t.Fatalf("whitespace not trimmed in DB: name=%q uid=%q discord_id=%q", name, uid, discordID)
	}
}

func TestHandleMember_DeleteSuccess(t *testing.T) {
	setupTest()

	// Delete Alice (who is not signed in)
	req, _ := http.NewRequest("DELETE", "/members/1", nil)
	rr := httptest.NewRecorder()

	handleMember(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200 OK, got %v; body=%s", rr.Code, rr.Body.String())
	}

	var resp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response JSON: %v", err)
	}
	if resp["message"] != "Member deleted successfully" {
		t.Fatalf("unexpected message: %v", resp["message"])
	}

	// Verify member is deleted from DB
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM members WHERE id = ?`, 1).Scan(&count)
	if err != nil {
		t.Fatalf("error querying DB: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected member to be deleted, but still exists")
	}

	// Verify cache was updated
	mu.RLock()
	_, exists := userDB["TEST_UID_1"]
	mu.RUnlock()
	if exists {
		t.Fatalf("member should be removed from cache")
	}
}

func TestHandleMember_DeleteNotFound(t *testing.T) {
	setupTest()

	// Try to delete non-existent member
	req, _ := http.NewRequest("DELETE", "/members/999", nil)
	rr := httptest.NewRecorder()

	handleMember(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected status 404 Not Found, got %v", rr.Code)
	}
}

func TestHandleMember_DeleteSignedInMember(t *testing.T) {
	setupTest()

	// Sign in Alice
	currentAttendees["TEST_UID_1"] = time.Now()

	// Try to delete Alice while she's signed in
	req, _ := http.NewRequest("DELETE", "/members/1", nil)
	rr := httptest.NewRecorder()

	handleMember(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("expected status 409 Conflict, got %v", rr.Code)
	}

	// Verify member still exists
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM members WHERE id = ?`, 1).Scan(&count)
	if err != nil {
		t.Fatalf("error querying DB: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected member to still exist")
	}
}

func TestHandleMember_DeleteInvalidID(t *testing.T) {
	setupTest()

	req, _ := http.NewRequest("DELETE", "/members/abc", nil)
	rr := httptest.NewRecorder()

	handleMember(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for invalid ID, got %v", rr.Code)
	}
}

func TestHandleMember_DeleteNoID(t *testing.T) {
	setupTest()

	req, _ := http.NewRequest("DELETE", "/members/", nil)
	rr := httptest.NewRecorder()

	handleMember(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for missing ID, got %v", rr.Code)
	}
}

func TestHandleMember_DeleteCascadesSessions(t *testing.T) {
	setupTest()

	// Add some sessions for Alice
	now := time.Now()
	_, err := db.Exec(`INSERT INTO sessions (member_id, signin_time, signout_time) VALUES (?, ?, ?)`,
		1, now.Add(-2*time.Hour).Format(time.RFC3339), now.Add(-1*time.Hour).Format(time.RFC3339))
	if err != nil {
		t.Fatalf("failed inserting session: %v", err)
	}

	// Verify session exists
	var sessionCount int
	err = db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE member_id = ?`, 1).Scan(&sessionCount)
	if err != nil {
		t.Fatalf("error querying sessions: %v", err)
	}
	if sessionCount != 1 {
		t.Fatalf("expected 1 session, got %d", sessionCount)
	}

	// Delete Alice
	req, _ := http.NewRequest("DELETE", "/members/1", nil)
	rr := httptest.NewRecorder()
	handleMember(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200 OK, got %v; body=%s", rr.Code, rr.Body.String())
	}

	// Verify sessions were cascaded deleted
	err = db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE member_id = ?`, 1).Scan(&sessionCount)
	if err != nil {
		t.Fatalf("error querying sessions: %v", err)
	}
	if sessionCount != 0 {
		t.Fatalf("expected 0 sessions after cascade delete, got %d", sessionCount)
	}
}
