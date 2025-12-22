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
