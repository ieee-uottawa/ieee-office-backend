package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// setupTest resets global state for a clean testing environment
func setupTest() {
	// Reset User DB
	userDB = map[string]Member{
		"TEST_UID_1": {ID: 1, Name: "Alice", UID: "TEST_UID_1"},
		"TEST_UID_2": {ID: 2, Name: "Bob", UID: "TEST_UID_2"},
	}

	// Reset Active Attendees
	currentAttendees = make(map[string]time.Time)

	// Reset Database (Use in-memory DB for speed)
	var err error
	db, err = sql.Open("sqlite", ":memory:")
	if err != nil {
		panic(err)
	}

	// Enable foreign keys and create tables
	if _, err := db.Exec(`PRAGMA foreign_keys = ON;`); err != nil {
		panic(err)
	}

	createMembersSQL := `CREATE TABLE IF NOT EXISTS members (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		uid TEXT NOT NULL UNIQUE
	);`
	if _, err := db.Exec(createMembersSQL); err != nil {
		panic(err)
	}

	// Seed members according to userDB
	for _, m := range userDB {
		if _, err := db.Exec(`INSERT INTO members (id, name, uid) VALUES (?, ?, ?)`, m.ID, m.Name, m.UID); err != nil {
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

	payload := []byte(`{"name":"Charlie","uid":"TEST_UID_3"}`)
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
	if m.Name != "Charlie" || m.UID != "TEST_UID_3" {
		t.Fatalf("unexpected member returned: %+v", m)
	}

	// Verify DB has the member
	row := db.QueryRow(`SELECT name, uid FROM members WHERE uid = ?`, "TEST_UID_3")
	var name, uid string
	if err := row.Scan(&name, &uid); err != nil {
		t.Fatalf("member not found in DB: %v", err)
	}
}

func TestHandleMembers_CreateDuplicate(t *testing.T) {
	setupTest()

	// First create
	payload := []byte(`{"name":"Dave","uid":"TEST_UID_4"}`)
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
