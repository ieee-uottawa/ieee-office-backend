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
