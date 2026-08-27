package simulator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// errLostRace is a 409 from the gateway: someone else took the seats. It is a
// normal outcome and is counted separately from errors, because during an on-sale
// almost every request loses and an error rate that includes them says nothing.
var errLostRace = errors.New("seats taken")

type apiEvent struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type apiSection struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Seats int    `json:"seats"`
}

type seat struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

func (s *Simulator) listEvents(ctx context.Context) ([]apiEvent, error) {
	var body struct {
		Events []apiEvent `json:"events"`
	}
	err := s.getJSON(ctx, "/api/events", &body)
	return body.Events, err
}

func (s *Simulator) sections(ctx context.Context, eventID string) ([]apiSection, error) {
	var body struct {
		Sections []apiSection `json:"sections"`
	}
	err := s.getJSON(ctx, "/api/events/"+eventID+"/sections", &body)
	return body.Sections, err
}

func (s *Simulator) seats(ctx context.Context, eventID, sectionID string) ([]seat, error) {
	var body struct {
		Seats []seat `json:"seats"`
	}
	err := s.getJSON(ctx, "/api/events/"+eventID+"/sections/"+sectionID, &body)
	return body.Seats, err
}

func (s *Simulator) hold(ctx context.Context, eventID string, seatIDs []string) (string, error) {
	var body struct {
		HoldID string `json:"hold_id"`
	}
	code, err := s.postJSON(ctx, "/api/holds", map[string]any{
		"event_id": eventID, "seat_ids": seatIDs,
	}, &body)
	if err != nil {
		return "", err
	}
	if code == http.StatusConflict {
		return "", errLostRace
	}
	if code != http.StatusCreated {
		return "", fmt.Errorf("hold: unexpected status %d", code)
	}
	return body.HoldID, nil
}

func (s *Simulator) order(ctx context.Context, holdID, eventID string, amountMinor int64) (string, error) {
	var body struct {
		State string `json:"state"`
	}
	code, err := s.postJSON(ctx, "/api/orders", map[string]any{
		"hold_id": holdID, "event_id": eventID,
		"user_id": newUUID(), "amount_minor": amountMinor,
	}, &body)
	if err != nil {
		return "", err
	}
	if code != http.StatusCreated {
		return "", fmt.Errorf("order: unexpected status %d", code)
	}
	return body.State, nil
}

func (s *Simulator) release(ctx context.Context, holdID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, s.baseURL+"/api/holds/"+holdID, nil)
	if err != nil {
		return err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("release: unexpected status %d", resp.StatusCode)
	}
	return nil
}

func (s *Simulator) getJSON(ctx context.Context, path string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+path, nil)
	if err != nil {
		return err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(into)
}

func (s *Simulator) postJSON(ctx context.Context, path string, body, into any) (int, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+path, bytes.NewReader(buf))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer drain(resp)

	if into != nil && resp.StatusCode < 500 {
		_ = json.NewDecoder(resp.Body).Decode(into)
	}
	return resp.StatusCode, nil
}

// drain reads and closes the body so the connection can be reused. Skipping this
// silently exhausts the connection pool under load, which then looks like the
// server slowing down.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}
