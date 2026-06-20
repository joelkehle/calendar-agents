package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/joelkehle/calendar-agents/internal/outlookcalendarwrite"
	"github.com/joelkehle/calendar-agents/internal/telemetry"
	calendarread "github.com/joelkehle/calendar-agents/pkg/calendarreadcontract"
)

// newWatcherHarness builds a travel harness whose read agent serves the given
// date-keyed events and whose write agent answers per writeFn (echo success
// by default).
func newWatcherHarness(t *testing.T, now time.Time, events map[string][]calendarread.Event, edit func(*Config), writeFn func(upstreamMessage) (string, bool)) *travelHarness {
	t.Helper()
	var h *travelHarness
	onSend := func(msg upstreamMessage) (string, bool) {
		return calendarByDate(t, events, writeFn)(msg)
	}
	h = newTravelHarness(t, now, edit, onSend)
	return h
}

func tick(t *testing.T, h *travelHarness) {
	t.Helper()
	h.agent.watcher.tick(context.Background())
}

func insertMessages(t *testing.T, h *travelHarness) []upstreamMessage {
	t.Helper()
	var out []upstreamMessage
	for _, msg := range h.writeMessages() {
		if decodeWriteRequest(t, msg).Action == "event-insert" {
			out = append(out, msg)
		}
	}
	return out
}

func patchMessages(t *testing.T, h *travelHarness) []upstreamMessage {
	t.Helper()
	var out []upstreamMessage
	for _, msg := range h.writeMessages() {
		if decodeWriteRequest(t, msg).Action == "event-patch" {
			out = append(out, msg)
		}
	}
	return out
}

func TestWatcherSkipList(t *testing.T) {
	t.Parallel()

	day := "2026-06-12"
	start := latStatic("2026-06-12T10:00:00")
	end := latStatic("2026-06-12T11:00:00")
	cases := []struct {
		name        string
		event       calendarread.Event
		wantInserts int
	}{
		{name: "all-day", event: wevt("e1", "Conference", start, end, withLocation("200 Medical Plaza"), asAllDay(day)), wantInserts: 0},
		{name: "transparent", event: wevt("e2", "FYI block", start, end, withLocation("200 Medical Plaza"), withTransparency("transparent")), wantInserts: 0},
		{name: "guard day summary", event: wevt("e3", "Meeting Quota Reached", start, end, withLocation("200 Medical Plaza")), wantInserts: 0},
		{name: "guard prefix summary", event: wevt("e4", "No more meetings after 3pm", start, end, withLocation("200 Medical Plaza")), wantInserts: 0},
		{name: "cancelled", event: wevt("e5", "[CANCELLED] Coffee", start, end, withLocation("200 Medical Plaza")), wantInserts: 0},
		{name: "travel prefix never gets travel-for-travel", event: wevt("e6", "Travel: 200 Medical Plaza (for 10:00)", start, end), wantInserts: 0},
		{name: "masked private sentinel", event: wevt("e7", "Private appointment", start, end, withLocation("200 Medical Plaza")), wantInserts: 0},
		{name: "already ended", event: wevt("e8", "Morning visit", latStatic("2026-06-11T09:00:00"), latStatic("2026-06-11T10:00:00"), withLocation("200 Medical Plaza")), wantInserts: 0},
		{name: "office location", event: wevt("e9", "Desk review", start, end, withLocation("10889 Wilshire Blvd")), wantInserts: 0},
		{name: "teams location is not offsite", event: wevt("e10", "Standup", start, end, withLocation("Microsoft Teams Meeting")), wantInserts: 0},
		{name: "zoom url is not offsite", event: wevt("e11", "Webinar", start, end, withLocation("https://ucla.zoom.us/j/123")), wantInserts: 0},
		{name: "yellow category is offsite", event: wevt("e12", "Site visit", start, end, withCategories("Yellow category")), wantInserts: 2},
		{name: "offsite location is offsite", event: wevt("e13", "Pickup", start, end, withLocation("200 Medical Plaza")), wantInserts: 2},
		{name: "yellow wins over virtual location", event: wevt("e14", "Hybrid visit", start, end, withLocation("Microsoft Teams Meeting"), withCategories("Yellow category")), wantInserts: 2},
		{name: "yellow rename fallback", event: wevt("e15", "Site visit", start, end, withCategories("Yellow - Offsite")), wantInserts: 2},
		{name: "yellow match is case-insensitive", event: wevt("e16", "Site visit", start, end, withCategories("YELLOW CATEGORY")), wantInserts: 2},
		{name: "unrelated category is not offsite", event: wevt("e17", "Desk work", start, end, withCategories("Blue category")), wantInserts: 0},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			eventDay := day
			if tc.name == "already ended" {
				eventDay = "2026-06-11"
			}
			h := newWatcherHarness(t, testNow(), map[string][]calendarread.Event{eventDay: {tc.event}}, nil, echoWriteSuccess(t))
			tick(t, h)
			if got := len(insertMessages(t, h)); got != tc.wantInserts {
				t.Fatalf("inserts = %d, want %d (writes: %#v)", got, tc.wantInserts, h.writeMessages())
			}
		})
	}
}

func TestWatcherCreatesMissingBlocks(t *testing.T) {
	t.Parallel()

	day := "2026-06-12"
	meeting := wevt("m1", "Pickup", latStatic("2026-06-12T10:00:00"), latStatic("2026-06-12T11:00:00"), withLocation("200 Medical Plaza"))

	t.Run("bare offsite meeting gets both blocks", func(t *testing.T) {
		t.Parallel()
		h := newWatcherHarness(t, testNow(), map[string][]calendarread.Event{day: {meeting}}, nil, echoWriteSuccess(t))
		tick(t, h)
		inserts := insertMessages(t, h)
		if len(inserts) != 2 {
			t.Fatalf("inserts = %d, want 2", len(inserts))
		}
		before := decodeWriteRequest(t, inserts[0])
		after := decodeWriteRequest(t, inserts[1])
		if *before.Event.Summary != "Travel: 200 Medical Plaza (for 10:00)" || *after.Event.Summary != "Travel: 200 Medical Plaza (return 11:00)" {
			t.Fatalf("summaries = %q / %q", *before.Event.Summary, *after.Event.Summary)
		}
		// Before leg departs from the office: 5 + 10 = 15.
		// With no later anchor, the return leg targets the same departure origin.
		if before.Event.Start.DateTime != "2026-06-12T09:45:00-07:00" || after.Event.End.DateTime != "2026-06-12T11:15:00-07:00" {
			t.Fatalf("block bounds = %s / %s", before.Event.Start.DateTime, after.Event.End.DateTime)
		}
		if after.Event.Location == nil || *after.Event.Location != "UCLA TDG office, 10889 Wilshire Blvd, Suite 920, Los Angeles, CA 90095-7191" {
			t.Fatalf("after return location = %#v", after.Event.Location)
		}
		for _, msg := range inserts {
			if !strings.HasPrefix(metaRequestID(msg), "schedw-") {
				t.Fatalf("meta key = %q, want schedw- prefix", metaRequestID(msg))
			}
		}
		if metaRequestID(inserts[0]) == metaRequestID(inserts[1]) {
			t.Fatal("before/after keys must differ")
		}
	})

	adjacency := []struct {
		name        string
		extra       calendarread.Event
		wantInserts int
	}{
		{
			name:        "adjacent travel block at the before tolerance edge",
			extra:       wevt("t1", "Travel: somewhere else (for 09:00)", latStatic("2026-06-12T09:10:00"), latStatic("2026-06-12T09:40:00")),
			wantInserts: 1, // before exists (T.end == M.start-20m), only after created
		},
		{
			// Non-grammar summary: adjacency is ownership-blind and
			// prefix-based; a grammar block here would instead be reattached
			// by §7.5(3), which is covered in TestWatcherReattach.
			name:        "travel block just outside the before tolerance",
			extra:       wevt("t2", "Travel: somewhere else", latStatic("2026-06-12T09:09:00"), latStatic("2026-06-12T09:39:00")),
			wantInserts: 2,
		},
		{
			name:        "adjacent travel block at the after tolerance edge",
			extra:       wevt("t3", "Travel: somewhere else (return 09:00)", latStatic("2026-06-12T11:20:00"), latStatic("2026-06-12T11:50:00")),
			wantInserts: 1, // after exists (T.start == M.end+20m), only before created
		},
		{
			name:        "travel block just outside the after tolerance",
			extra:       wevt("t4", "Travel: somewhere else", latStatic("2026-06-12T11:21:00"), latStatic("2026-06-12T11:51:00")),
			wantInserts: 2,
		},
	}
	for _, tc := range adjacency {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newWatcherHarness(t, testNow(), map[string][]calendarread.Event{day: {meeting, tc.extra}}, nil, echoWriteSuccess(t))
			tick(t, h)
			if got := len(insertMessages(t, h)); got != tc.wantInserts {
				t.Fatalf("inserts = %d, want %d", got, tc.wantInserts)
			}
		})
	}
}

func TestWatcherRepairsAttachedTravelBlockLocation(t *testing.T) {
	t.Parallel()

	day := "2026-06-22"
	meeting := wevt("m-west", "Tentative - In Person with Rajita and team discuss IP and RIA", latStatic("2026-06-22T12:00:00"), latStatic("2026-06-22T13:00:00"), withLocation("UCLA West Medical Building (1010 Veteran Ave, Los Angeles, CA, United States)"))
	before := wevt("t-before", "Travel: UCLA West Medical Building (1010 Veteran Ave, Los Angeles, C (for 12:00)", latStatic("2026-06-22T11:45:00"), latStatic("2026-06-22T12:00:00"))
	after := wevt("t-after", "Travel: UCLA West Medical Building (1010 Veteran Ave, Los Angeles, C (return 12:00)", latStatic("2026-06-22T13:00:00"), latStatic("2026-06-22T13:30:00"))
	h := newWatcherHarness(t, latStatic("2026-06-19T18:00:00"), map[string][]calendarread.Event{day: {meeting, before, after}}, nil, echoWriteSuccess(t))

	tick(t, h)

	if got := len(insertMessages(t, h)); got != 0 {
		t.Fatalf("inserts = %d, want 0", got)
	}
	patches := patchMessages(t, h)
	if len(patches) != 2 {
		t.Fatalf("patches = %d, want 2: %#v", len(patches), h.writeMessages())
	}
	beforePatch := decodeWriteRequest(t, patches[0])
	afterPatch := decodeWriteRequest(t, patches[1])
	if *beforePatch.Event.Summary != "Travel: UCLA West Medical Building (for 12:00)" {
		t.Fatalf("before summary = %q", *beforePatch.Event.Summary)
	}
	if beforePatch.Event.Location == nil || *beforePatch.Event.Location != "UCLA West Medical Building, 1010 Veteran Ave, Los Angeles, CA 90095" {
		t.Fatalf("before location = %#v", beforePatch.Event.Location)
	}
	if beforePatch.Event.Start.DateTime != "2026-06-22T11:45:00-07:00" || beforePatch.Event.End.DateTime != "2026-06-22T12:00:00-07:00" {
		t.Fatalf("before repair bounds = %s/%s", beforePatch.Event.Start.DateTime, beforePatch.Event.End.DateTime)
	}
	if *afterPatch.Event.Summary != "Travel: UCLA West Medical Building (return 13:00)" {
		t.Fatalf("after summary = %q", *afterPatch.Event.Summary)
	}
	if afterPatch.Event.Location == nil || *afterPatch.Event.Location != "UCLA TDG office, 10889 Wilshire Blvd, Suite 920, Los Angeles, CA 90095-7191" {
		t.Fatalf("after location = %#v", afterPatch.Event.Location)
	}
	if afterPatch.Event.Start.DateTime != "2026-06-22T13:00:00-07:00" || afterPatch.Event.End.DateTime != "2026-06-22T13:30:00-07:00" {
		t.Fatalf("after repair bounds = %s/%s", afterPatch.Event.Start.DateTime, afterPatch.Event.End.DateTime)
	}
	if !strings.Contains(*afterPatch.Event.Description, "Destination: UCLA TDG office, 10889 Wilshire Blvd, Suite 920, Los Angeles, CA 90095-7191") {
		t.Fatalf("after description = %q", *afterPatch.Event.Description)
	}
	if got := metricValue(t, h.metrics, "watch_travel_repaired"); got != 2 {
		t.Fatalf("watch_travel_repaired = %d, want 2", got)
	}
}

func TestWatcherProposesCommunicationForCompressedPriorityTravel(t *testing.T) {
	t.Parallel()

	day := "2026-06-22"
	carol := wevt("carol-1x1", "Carol & Joel 1-1", latStatic("2026-06-22T11:00:00"), latStatic("2026-06-22T11:45:00"),
		withAttendees(
			calendarread.EventAttendee{DisplayName: "Carol", Email: "carol@example.com"},
			calendarread.EventAttendee{DisplayName: "Joel Kehle", Email: "joel@kehle.com", Self: true},
		),
	)
	meeting := wevt("m-west", "Tentative - In Person with Rajita and team discuss IP and RIA", latStatic("2026-06-22T12:00:00"), latStatic("2026-06-22T13:00:00"),
		withLocation("UCLA West Medical Building (1010 Veteran Ave, Los Angeles, CA, United States)"),
		withAttendees(calendarread.EventAttendee{DisplayName: "Jeanson, Josh", Email: "jjeanson@example.edu", Organizer: true}),
	)

	t.Run("attached short block is not treated as success", func(t *testing.T) {
		t.Parallel()
		before := wevt("t-before", "Travel: UCLA West Medical Building (for 12:00)", latStatic("2026-06-22T11:45:00"), latStatic("2026-06-22T12:00:00"), withLocation("UCLA West Medical Building, 1010 Veteran Ave, Los Angeles, CA 90095"))
		h := newWatcherHarness(t, latStatic("2026-06-19T18:00:00"), map[string][]calendarread.Event{day: {carol, meeting, before}}, nil, echoWriteSuccess(t))

		tick(t, h)

		proposals := h.agent.watcher.lastCommunicationProposals
		if len(proposals) != 1 {
			t.Fatalf("proposals = %d, want 1: %#v", len(proposals), proposals)
		}
		proposal := proposals[0]
		if proposal.RecipientLabel != "Carol" || proposal.RecipientEmail != "carol@example.com" {
			t.Fatalf("recipient = %q <%s>, want Carol <carol@example.com>", proposal.RecipientLabel, proposal.RecipientEmail)
		}
		if proposal.RequiredMinutes != 30 || proposal.AvailableMinutes != 30 {
			t.Fatalf("minutes = required %d available %d, want 30/30", proposal.RequiredMinutes, proposal.AvailableMinutes)
		}
		if !proposal.DesiredLeaveTime.Equal(latStatic("2026-06-22T11:30:00")) || !proposal.CurrentLeaveTime.Equal(latStatic("2026-06-22T11:45:00")) {
			t.Fatalf("leave times = desired %s current %s", proposal.DesiredLeaveTime, proposal.CurrentLeaveTime)
		}
		if !strings.Contains(proposal.Body, "I need to drop at 11:30 AM because I have an obligation with Josh") {
			t.Fatalf("proposal body = %q", proposal.Body)
		}
		if got := metricValue(t, h.metrics, "watch_travel_communication_proposed"); got != 1 {
			t.Fatalf("watch_travel_communication_proposed = %d, want 1", got)
		}
	})

	t.Run("new compressed block also creates a proposal", func(t *testing.T) {
		t.Parallel()
		h := newWatcherHarness(t, latStatic("2026-06-19T18:00:00"), map[string][]calendarread.Event{day: {carol, meeting}}, nil, echoWriteSuccess(t))

		tick(t, h)

		proposals := h.agent.watcher.lastCommunicationProposals
		if len(proposals) != 1 {
			t.Fatalf("proposals = %d, want 1: %#v", len(proposals), proposals)
		}
		inserts := insertMessages(t, h)
		if len(inserts) != 2 {
			t.Fatalf("inserts = %d, want 2", len(inserts))
		}
		before := decodeWriteRequest(t, inserts[0])
		if before.Event.Start.DateTime != "2026-06-22T11:45:00-07:00" || before.Event.End.DateTime != "2026-06-22T12:00:00-07:00" {
			t.Fatalf("compressed before block = %s/%s, want 11:45-12:00", before.Event.Start.DateTime, before.Event.End.DateTime)
		}
	})
}

func TestWatcherRecalculatesSummerSolsticeTravel(t *testing.T) {
	t.Parallel()

	day := "2026-06-18"
	events := []calendarread.Event{
		wevt("house", "Joel housesitting for Jaqueline and Monte", latStatic("2026-06-18T00:00:00"), latStatic("2026-06-19T00:00:00"), asAllDay(day)),
		wevt("office-close", "TDG Office closes early at 3pm", latStatic("2026-06-18T00:00:00"), latStatic("2026-06-19T00:00:00"), asAllDay(day)),
		wevt("office", "TDG office work", latStatic("2026-06-18T14:00:00"), latStatic("2026-06-18T14:30:00"), withLocation("10889 Wilshire Blvd")),
		wevt("zoom", "Tadeo, Keerthi, Ryan, Tanika, Alexandra, and Joel - Follow up", latStatic("2026-06-18T16:00:00"), latStatic("2026-06-18T16:30:00"), withLocation("https://ucla.zoom.us/j/123")),
		wevt("mcc", "Summer Solstice Committee Celebration", latStatic("2026-06-18T18:00:00"), latStatic("2026-06-18T20:00:00"), withLocation("Manhattan Country Club (1330 Manhattan Ave, Manhattan Beach, CA 90266)")),
	}
	h := newWatcherHarness(t, latStatic("2026-06-18T13:00:00"), map[string][]calendarread.Event{day: events}, nil, echoWriteSuccess(t))

	tick(t, h)

	inserts := insertMessages(t, h)
	if len(inserts) != 2 {
		t.Fatalf("inserts = %d, want 2: %#v", len(inserts), h.writeMessages())
	}
	before := decodeWriteRequest(t, inserts[0])
	after := decodeWriteRequest(t, inserts[1])
	if *before.Event.Summary != "Travel: Manhattan Country Club (for 18:00)" {
		t.Fatalf("before summary = %q", *before.Event.Summary)
	}
	if *after.Event.Summary != "Travel: Manhattan Country Club (return 20:00)" {
		t.Fatalf("after summary = %q", *after.Event.Summary)
	}
	if before.Event.Location == nil || *before.Event.Location != "Manhattan Country Club, 1330 Parkview Avenue, Manhattan Beach, CA 90266" {
		t.Fatalf("before travel location = %#v", before.Event.Location)
	}
	if after.Event.Location == nil || *after.Event.Location != "Monte & Jacqueline's (housesitting), 9121 Alto Cedro Drive, Beverly Hills, CA 90210" {
		t.Fatalf("after travel location = %#v", after.Event.Location)
	}
	if !strings.Contains(*before.Event.Description, "Destination: Manhattan Country Club, 1330 Parkview Avenue, Manhattan Beach, CA 90266") {
		t.Fatalf("before travel description = %q", *before.Event.Description)
	}
	if !strings.Contains(*after.Event.Description, "Destination: Monte & Jacqueline's (housesitting), 9121 Alto Cedro Drive, Beverly Hills, CA 90210") {
		t.Fatalf("after travel description = %q", *after.Event.Description)
	}
	if before.Event.Start.DateTime != "2026-06-18T16:45:00-07:00" || before.Event.End.DateTime != "2026-06-18T18:00:00-07:00" {
		t.Fatalf("before block = %s/%s, want 16:45-18:00", before.Event.Start.DateTime, before.Event.End.DateTime)
	}
	if after.Event.Start.DateTime != "2026-06-18T20:00:00-07:00" || after.Event.End.DateTime != "2026-06-18T21:00:00-07:00" {
		t.Fatalf("after block = %s/%s, want 20:00-21:00", after.Event.Start.DateTime, after.Event.End.DateTime)
	}
}

func TestWatcherInfersDavidKTivertonLocation(t *testing.T) {
	t.Parallel()

	now := latStatic("2026-06-23T09:00:00")
	day := "2026-06-24" // Wednesday
	start := latStatic("2026-06-24T16:30:00")
	end := latStatic("2026-06-24T17:30:00")
	davidAttendee := withAttendees(calendarread.EventAttendee{DisplayName: "David Kronemyer", Email: "dkronemyer@example.com"})

	t.Run("blank visible Wednesday David K meeting uses Tiverton", func(t *testing.T) {
		t.Parallel()
		meeting := wevt("dk-blank", "David K", start, end, davidAttendee)
		h := newWatcherHarness(t, now, map[string][]calendarread.Event{day: {meeting}}, nil, echoWriteSuccess(t))

		tick(t, h)

		inserts := insertMessages(t, h)
		if len(inserts) != 2 {
			t.Fatalf("inserts = %d, want 2: %#v", len(inserts), h.writeMessages())
		}
		before := decodeWriteRequest(t, inserts[0])
		after := decodeWriteRequest(t, inserts[1])
		if *before.Event.Summary != "Travel: 805 Tiverton Ave (for 16:30)" {
			t.Fatalf("before summary = %q", *before.Event.Summary)
		}
		if before.Event.Location == nil || *before.Event.Location != "805 Tiverton Ave, Los Angeles, CA 90024" {
			t.Fatalf("before location = %#v", before.Event.Location)
		}
		if before.Event.Start.DateTime != "2026-06-24T16:00:00-07:00" || before.Event.End.DateTime != "2026-06-24T16:30:00-07:00" {
			t.Fatalf("before block = %s/%s, want 16:00-16:30", before.Event.Start.DateTime, before.Event.End.DateTime)
		}
		if *after.Event.Summary != "Travel: 805 Tiverton Ave (return 17:30)" {
			t.Fatalf("after summary = %q", *after.Event.Summary)
		}
		if after.Event.Location == nil || *after.Event.Location != "UCLA TDG office, 10889 Wilshire Blvd, Suite 920, Los Angeles, CA 90095-7191" {
			t.Fatalf("after location = %#v", after.Event.Location)
		}
	})

	t.Run("plain virtual David K meeting stays virtual", func(t *testing.T) {
		t.Parallel()
		meeting := wevt("dk-virtual", "David K", start, end, withLocation("Microsoft Teams Meeting"), davidAttendee)
		h := newWatcherHarness(t, now, map[string][]calendarread.Event{day: {meeting}}, nil, echoWriteSuccess(t))

		tick(t, h)

		if got := len(insertMessages(t, h)); got != 0 {
			t.Fatalf("inserts = %d, want 0", got)
		}
	})

	t.Run("virtual with face-to-face signal uses Tiverton", func(t *testing.T) {
		t.Parallel()
		meeting := wevt("dk-f2f", "Face to Face with David K", start, end, withLocation("https://ucla.zoom.us/j/123"), davidAttendee)
		h := newWatcherHarness(t, now, map[string][]calendarread.Event{day: {meeting}}, nil, echoWriteSuccess(t))

		tick(t, h)

		inserts := insertMessages(t, h)
		if len(inserts) != 2 {
			t.Fatalf("inserts = %d, want 2: %#v", len(inserts), h.writeMessages())
		}
		before := decodeWriteRequest(t, inserts[0])
		if before.Event.Location == nil || *before.Event.Location != "805 Tiverton Ave, Los Angeles, CA 90024" {
			t.Fatalf("before location = %#v", before.Event.Location)
		}
	})

	t.Run("Thursday David K does not infer", func(t *testing.T) {
		t.Parallel()
		thursday := "2026-06-25"
		meeting := wevt("dk-thursday", "David K", latStatic("2026-06-25T16:30:00"), latStatic("2026-06-25T17:30:00"), davidAttendee)
		h := newWatcherHarness(t, now, map[string][]calendarread.Event{thursday: {meeting}}, nil, echoWriteSuccess(t))

		tick(t, h)

		if got := len(insertMessages(t, h)); got != 0 {
			t.Fatalf("inserts = %d, want 0", got)
		}
	})
}

func TestWatcherShrinkAndSkip(t *testing.T) {
	t.Parallel()

	day := "2026-06-12"
	meeting := wevt("m1", "Pickup", latStatic("2026-06-12T10:00:00"), latStatic("2026-06-12T11:00:00"), withLocation("200 Medical Plaza"))

	t.Run("after side trimmed and rounded down to five minutes", func(t *testing.T) {
		t.Parallel()
		next := wevt("m2", "1:1", latStatic("2026-06-12T11:12:00"), latStatic("2026-06-12T12:00:00"))
		h := newWatcherHarness(t, testNow(), map[string][]calendarread.Event{day: {meeting, next}}, nil, echoWriteSuccess(t))
		tick(t, h)
		inserts := insertMessages(t, h)
		if len(inserts) != 2 {
			t.Fatalf("inserts = %d, want 2", len(inserts))
		}
		after := decodeWriteRequest(t, inserts[1])
		if after.Event.Start.DateTime != "2026-06-12T11:00:00-07:00" || after.Event.End.DateTime != "2026-06-12T11:10:00-07:00" {
			t.Fatalf("after block = %s/%s, want 11:00-11:10 (12 min gap rounded to 10)", after.Event.Start.DateTime, after.Event.End.DateTime)
		}
	})

	t.Run("after side under ten minutes is skipped", func(t *testing.T) {
		t.Parallel()
		next := wevt("m2", "1:1", latStatic("2026-06-12T11:08:00"), latStatic("2026-06-12T12:00:00"))
		h := newWatcherHarness(t, testNow(), map[string][]calendarread.Event{day: {meeting, next}}, nil, echoWriteSuccess(t))
		tick(t, h)
		if got := len(insertMessages(t, h)); got != 1 {
			t.Fatalf("inserts = %d, want 1 (before only)", got)
		}
		if got := metricValue(t, h.metrics, "watch_travel_skipped"); got != 1 {
			t.Fatalf("watch_travel_skipped = %d, want 1", got)
		}
	})

	t.Run("before side clamps to now plus one minute", func(t *testing.T) {
		t.Parallel()
		// Today 16:20 meeting (now 16:00), unknown venue => 30-min block
		// [15:50, 16:20) clamps to [16:01, 16:20) => 19 min => 15 => 16:05.
		today := wevt("m3", "Urgent visit", latStatic("2026-06-11T16:20:00"), latStatic("2026-06-11T17:00:00"), withLocation("Cedars-Sinai"))
		h := newWatcherHarness(t, testNow(), map[string][]calendarread.Event{"2026-06-11": {today}}, nil, echoWriteSuccess(t))
		tick(t, h)
		inserts := insertMessages(t, h)
		if len(inserts) != 2 {
			t.Fatalf("inserts = %d, want 2", len(inserts))
		}
		before := decodeWriteRequest(t, inserts[0])
		if before.Event.Start.DateTime != "2026-06-11T16:05:00-07:00" || before.Event.End.DateTime != "2026-06-11T16:20:00-07:00" {
			t.Fatalf("before block = %s/%s, want clamped 16:05-16:20", before.Event.Start.DateTime, before.Event.End.DateTime)
		}
	})

	t.Run("six AM floor clamps the before block", func(t *testing.T) {
		t.Parallel()
		early := wevt("m4", "Early pickup", latStatic("2026-06-12T06:15:00"), latStatic("2026-06-12T07:00:00"), withLocation("200 Medical Plaza"))
		h := newWatcherHarness(t, testNow(), map[string][]calendarread.Event{day: {early}}, nil, echoWriteSuccess(t))
		tick(t, h)
		inserts := insertMessages(t, h)
		if len(inserts) != 2 {
			t.Fatalf("inserts = %d, want 2", len(inserts))
		}
		before := decodeWriteRequest(t, inserts[0])
		if before.Event.Start.DateTime != "2026-06-12T06:00:00-07:00" {
			t.Fatalf("before block start = %s, want clamped 06:00", before.Event.Start.DateTime)
		}
	})

	t.Run("ten PM ceiling clamps the after block", func(t *testing.T) {
		t.Parallel()
		late := wevt("m5", "Late dinner", latStatic("2026-06-12T21:00:00"), latStatic("2026-06-12T21:50:00"), withLocation("Cedars-Sinai"))
		h := newWatcherHarness(t, testNow(), map[string][]calendarread.Event{day: {late}}, nil, echoWriteSuccess(t))
		tick(t, h)
		var after *outlookcalendarwrite.Request
		for _, msg := range insertMessages(t, h) {
			req := decodeWriteRequest(t, msg)
			if strings.Contains(*req.Event.Summary, "(return ") {
				after = &req
			}
		}
		if after == nil {
			t.Fatal("after block not created")
		}
		if after.Event.Start.DateTime != "2026-06-12T21:50:00-07:00" || after.Event.End.DateTime != "2026-06-12T22:00:00-07:00" {
			t.Fatalf("after block = %s/%s, want clamped 21:50-22:00", after.Event.Start.DateTime, after.Event.End.DateTime)
		}
	})

	t.Run("ceiling leaves under ten minutes and is skipped", func(t *testing.T) {
		t.Parallel()
		late := wevt("m6", "Later dinner", latStatic("2026-06-12T21:00:00"), latStatic("2026-06-12T21:55:00"), withLocation("Cedars-Sinai"))
		h := newWatcherHarness(t, testNow(), map[string][]calendarread.Event{day: {late}}, nil, echoWriteSuccess(t))
		tick(t, h)
		for _, msg := range insertMessages(t, h) {
			req := decodeWriteRequest(t, msg)
			if strings.Contains(*req.Event.Summary, "(return ") {
				t.Fatalf("after block must be skipped: %#v", req)
			}
		}
		if got := metricValue(t, h.metrics, "watch_travel_skipped"); got != 1 {
			t.Fatalf("watch_travel_skipped = %d, want 1", got)
		}
	})

	t.Run("fully blocked side is skipped with metric", func(t *testing.T) {
		t.Parallel()
		prep := wevt("m7", "Prep session", latStatic("2026-06-12T09:30:00"), latStatic("2026-06-12T10:00:00"))
		h := newWatcherHarness(t, testNow(), map[string][]calendarread.Event{day: {meeting, prep}}, nil, echoWriteSuccess(t))
		tick(t, h)
		for _, msg := range insertMessages(t, h) {
			req := decodeWriteRequest(t, msg)
			if strings.Contains(*req.Event.Summary, "(for ") {
				t.Fatalf("blocked before side must not be created: %#v", req)
			}
		}
		if got := metricValue(t, h.metrics, "watch_travel_skipped"); got != 1 {
			t.Fatalf("watch_travel_skipped = %d, want 1", got)
		}
	})

	t.Run("meeting buffer does not block explicit travel", func(t *testing.T) {
		t.Parallel()
		buffer := wevt("buf1", "Meeting Buffer", latStatic("2026-06-12T09:30:00"), latStatic("2026-06-12T10:00:00"))
		h := newWatcherHarness(t, testNow(), map[string][]calendarread.Event{day: {meeting, buffer}}, nil, echoWriteSuccess(t))
		tick(t, h)
		inserts := insertMessages(t, h)
		if len(inserts) != 2 {
			t.Fatalf("inserts = %d, want 2 (buffer is a placeholder, not a blocker)", len(inserts))
		}
		before := decodeWriteRequest(t, inserts[0])
		if before.Event.Start.DateTime != "2026-06-12T09:45:00-07:00" || before.Event.End.DateTime != "2026-06-12T10:00:00-07:00" {
			t.Fatalf("before block = %s/%s, want 09:45-10:00", before.Event.Start.DateTime, before.Event.End.DateTime)
		}
	})
}

func TestWatcherYellowNoLocation(t *testing.T) {
	t.Parallel()

	meeting := wevt("m1", "Mystery errand", latStatic("2026-06-12T10:00:00"), latStatic("2026-06-12T11:00:00"), withCategories("Yellow category"))
	h := newWatcherHarness(t, testNow(), map[string][]calendarread.Event{"2026-06-12": {meeting}}, nil, echoWriteSuccess(t))
	tick(t, h)
	inserts := insertMessages(t, h)
	if len(inserts) != 2 {
		t.Fatalf("inserts = %d, want 2", len(inserts))
	}
	before := decodeWriteRequest(t, inserts[0])
	if *before.Event.Summary != "Travel: offsite (for 10:00)" {
		t.Fatalf("before summary = %q, want dest offsite", *before.Event.Summary)
	}
	// Default estimate 30 minutes.
	if before.Event.Start.DateTime != "2026-06-12T09:30:00-07:00" {
		t.Fatalf("before block start = %s, want 09:30 (default 30 min)", before.Event.Start.DateTime)
	}
}

func TestWatcherNoOrigin(t *testing.T) {
	t.Parallel()

	// Friday 2026-07-03 now; Saturday 2026-07-04 meeting; the closed fixture
	// has no residence after 2026-06-30 and weekends get no office fallback.
	now := latStatic("2026-07-03T16:00:00")
	meeting := wevt("m1", "Holiday visit", latStatic("2026-07-04T10:00:00"), latStatic("2026-07-04T11:00:00"), withLocation("200 Medical Plaza"))
	h := newWatcherHarness(t, now, map[string][]calendarread.Event{"2026-07-04": {meeting}}, func(cfg *Config) {
		cfg.LocationsPath = "testdata/locations_closed.json"
	}, echoWriteSuccess(t))
	tick(t, h)
	if got := len(h.writeMessages()); got != 0 {
		t.Fatalf("writes = %d, want 0", got)
	}
	if got := metricValue(t, h.metrics, "watch_travel_skipped"); got != 1 {
		t.Fatalf("watch_travel_skipped = %d, want 1", got)
	}
}

func TestWatcherKeyEmbedsInterval(t *testing.T) {
	t.Parallel()

	day := "2026-06-12"
	// Tick 1: original meeting, capture the after-side insert key.
	original := wevt("m1", "Pickup", latStatic("2026-06-12T10:00:00"), latStatic("2026-06-12T11:00:00"), withLocation("200 Medical Plaza"))
	h1 := newWatcherHarness(t, testNow(), map[string][]calendarread.Event{day: {original}}, nil, echoWriteSuccess(t))
	tick(t, h1)
	inserts1 := insertMessages(t, h1)
	if len(inserts1) != 2 {
		t.Fatalf("tick1 inserts = %d, want 2", len(inserts1))
	}
	var afterKey1 string
	for _, msg := range inserts1 {
		if strings.Contains(*decodeWriteRequest(t, msg).Event.Summary, "(return ") {
			afterKey1 = metaRequestID(msg)
		}
	}

	// Tick 2 (fresh watcher): parent END extended (start unchanged); the
	// original blocks are on the calendar.
	extended := wevt("m1", "Pickup", latStatic("2026-06-12T10:00:00"), latStatic("2026-06-12T11:30:00"), withLocation("200 Medical Plaza"))
	beforeBlock := wevt("tb1", "Travel: 200 Medical Plaza (for 10:00)", latStatic("2026-06-12T09:45:00"), latStatic("2026-06-12T10:00:00"), withLocation("200 Medical Plaza, Los Angeles, CA 90024"))
	afterBlock := wevt("tb2", "Travel: 200 Medical Plaza (return 10:00)", latStatic("2026-06-12T11:00:00"), latStatic("2026-06-12T11:15:00"))
	h2 := newWatcherHarness(t, testNow(), map[string][]calendarread.Event{day: {extended, beforeBlock, afterBlock}}, nil, echoWriteSuccess(t))
	tick(t, h2)

	inserts2 := insertMessages(t, h2)
	if len(inserts2) != 1 {
		t.Fatalf("tick2 inserts = %d, want 1 (fresh after block)", len(inserts2))
	}
	freshAfter := decodeWriteRequest(t, inserts2[0])
	if !strings.Contains(*freshAfter.Event.Summary, "(return 11:30)") {
		t.Fatalf("fresh insert = %#v", freshAfter)
	}
	if metaRequestID(inserts2[0]) == afterKey1 {
		t.Fatal("extended parent END must yield a NEW meta.request_id (key embeds the interval)")
	}
	patches := patchMessages(t, h2)
	if len(patches) != 1 {
		t.Fatalf("tick2 patches = %d, want 1 (stale block cancelled)", len(patches))
	}
	cancel := decodeWriteRequest(t, patches[0])
	if cancel.EventID != "entry-tb2" || *cancel.Event.Summary != outlookcalendarwrite.CancelledPrefix {
		t.Fatalf("stale block handling = %#v, want cancel of entry-tb2", cancel)
	}
}

func TestWatcherOrphanCancel(t *testing.T) {
	t.Parallel()

	orphan := wevt("tb1", "Travel: 200 Medical Plaza (for 10:00)", latStatic("2026-06-12T09:45:00"), latStatic("2026-06-12T10:00:00"))
	h := newWatcherHarness(t, testNow(), map[string][]calendarread.Event{"2026-06-12": {orphan}}, nil, echoWriteSuccess(t))
	tick(t, h)
	writes := h.writeMessages()
	if len(writes) != 1 {
		t.Fatalf("writes = %d, want 1 cancel", len(writes))
	}
	cancel := decodeWriteRequest(t, writes[0])
	if cancel.Action != "event-patch" || cancel.EventID != "entry-tb1" ||
		*cancel.Event.Summary != outlookcalendarwrite.CancelledPrefix || *cancel.Event.ShowAs != "free" {
		t.Fatalf("cancel = %#v", cancel)
	}
	if !strings.HasPrefix(metaRequestID(writes[0]), "schedw-cancel-") {
		t.Fatalf("cancel key = %q", metaRequestID(writes[0]))
	}
}

func TestWatcherAnchorsToAnyBusy(t *testing.T) {
	t.Parallel()

	// Travel blocks bracketing a scheduler HOLD (busy, no location, no
	// category) must be left alone: §7.6 regression guard.
	hold := wevt("h1", "Joel + Fable: working session", latStatic("2026-06-12T10:00:00"), latStatic("2026-06-12T11:00:00"))
	before := wevt("tb1", "Travel: 200 Medical Plaza (for 10:00)", latStatic("2026-06-12T09:45:00"), latStatic("2026-06-12T10:00:00"))
	after := wevt("tb2", "Travel: 200 Medical Plaza (return 10:00)", latStatic("2026-06-12T11:00:00"), latStatic("2026-06-12T11:15:00"))
	h := newWatcherHarness(t, testNow(), map[string][]calendarread.Event{"2026-06-12": {hold, before, after}}, nil, echoWriteSuccess(t))
	tick(t, h)
	if got := len(h.writeMessages()); got != 0 {
		t.Fatalf("writes = %d, want 0 (anchored blocks untouched)", got)
	}
}

func TestWatcherReattach(t *testing.T) {
	t.Parallel()

	day := "2026-06-12"
	staleBefore := wevt("tb1", "Travel: 200 Medical Plaza (for 10:00)", latStatic("2026-06-12T09:45:00"), latStatic("2026-06-12T10:00:00"))

	t.Run("moved parent gets a recomputed patch", func(t *testing.T) {
		t.Parallel()
		moved := wevt("m1", "Pickup", latStatic("2026-06-12T14:00:00"), latStatic("2026-06-12T15:00:00"), withLocation("200 Medical Plaza"))
		h := newWatcherHarness(t, testNow(), map[string][]calendarread.Event{day: {moved, staleBefore}}, nil, echoWriteSuccess(t))
		tick(t, h)
		patches := patchMessages(t, h)
		if len(patches) != 1 {
			t.Fatalf("patches = %d, want 1 reattach", len(patches))
		}
		move := decodeWriteRequest(t, patches[0])
		if move.EventID != "entry-tb1" {
			t.Fatalf("reattach target = %q", move.EventID)
		}
		if *move.Event.Summary != "Travel: 200 Medical Plaza (for 14:00)" {
			t.Fatalf("reattach summary = %q", *move.Event.Summary)
		}
		// 14:00 weekday meeting departs from the office: 15-minute block.
		if move.Event.Start.DateTime != "2026-06-12T13:45:00-07:00" || move.Event.End.DateTime != "2026-06-12T14:00:00-07:00" {
			t.Fatalf("reattach interval = %s/%s", move.Event.Start.DateTime, move.Event.End.DateTime)
		}
		if !strings.HasPrefix(metaRequestID(patches[0]), "schedw-move-") {
			t.Fatalf("reattach key = %q", metaRequestID(patches[0]))
		}
		// The reattach claims the before side; only the after side is inserted.
		inserts := insertMessages(t, h)
		if len(inserts) != 1 || !strings.Contains(*decodeWriteRequest(t, inserts[0]).Event.Summary, "(return 15:00)") {
			t.Fatalf("inserts = %#v, want only the after side", inserts)
		}
	})

	t.Run("multiple candidates do nothing and count ambiguous", func(t *testing.T) {
		t.Parallel()
		m1 := wevt("m1", "Pickup", latStatic("2026-06-12T14:00:00"), latStatic("2026-06-12T15:00:00"), withLocation("200 Medical Plaza"))
		m2 := wevt("m2", "Drop-off", latStatic("2026-06-12T16:00:00"), latStatic("2026-06-12T17:00:00"), withLocation("Cedars-Sinai"))
		h := newWatcherHarness(t, testNow(), map[string][]calendarread.Event{day: {m1, m2, staleBefore}}, nil, echoWriteSuccess(t))
		tick(t, h)
		for _, msg := range patchMessages(t, h) {
			if decodeWriteRequest(t, msg).EventID == "entry-tb1" {
				t.Fatalf("ambiguous block must not be mutated: %#v", msg)
			}
		}
		if got := metricValue(t, h.metrics, "watch_travel_ambiguous"); got != 1 {
			t.Fatalf("watch_travel_ambiguous = %d, want 1", got)
		}
	})

	t.Run("unfittable recomputed side cancels the block", func(t *testing.T) {
		t.Parallel()
		moved := wevt("m1", "Pickup", latStatic("2026-06-12T14:00:00"), latStatic("2026-06-12T15:00:00"), withLocation("200 Medical Plaza"))
		blocker := wevt("m3", "Back-to-back", latStatic("2026-06-12T13:00:00"), latStatic("2026-06-12T14:00:00"))
		h := newWatcherHarness(t, testNow(), map[string][]calendarread.Event{day: {moved, blocker, staleBefore}}, nil, echoWriteSuccess(t))
		tick(t, h)
		var cancelled bool
		for _, msg := range patchMessages(t, h) {
			req := decodeWriteRequest(t, msg)
			if req.EventID == "entry-tb1" && req.Event.Summary != nil && *req.Event.Summary == outlookcalendarwrite.CancelledPrefix {
				cancelled = true
			}
		}
		if !cancelled {
			t.Fatalf("unfittable reattach must fall through to cancel: %#v", h.writeMessages())
		}
	})
}

func TestWatcherNoEntryID(t *testing.T) {
	t.Parallel()

	orphan := wevt("tb1", "Travel: somewhere (for 09:00)", latStatic("2026-06-12T08:30:00"), latStatic("2026-06-12T09:00:00"), withoutEntryID())
	meeting := wevt("m1", "Pickup", latStatic("2026-06-12T13:00:00"), latStatic("2026-06-12T14:00:00"), withLocation("200 Medical Plaza"))
	adjacent := wevt("tb2", "Travel: 200 Medical Plaza (for 13:00)", latStatic("2026-06-12T12:30:00"), latStatic("2026-06-12T13:00:00"), withoutEntryID())
	h := newWatcherHarness(t, testNow(), map[string][]calendarread.Event{"2026-06-12": {orphan, meeting, adjacent}}, nil, echoWriteSuccess(t))
	tick(t, h)

	if got := len(patchMessages(t, h)); got != 0 {
		t.Fatalf("patches = %d, want 0 (no entry_id, no mutation)", got)
	}
	if got := metricValue(t, h.metrics, "watch_travel_no_entry_id"); got != 1 {
		t.Fatalf("watch_travel_no_entry_id = %d, want 1", got)
	}
	// The entry-id-less adjacent block still counts for adjacency: only the
	// after side of the meeting is created.
	inserts := insertMessages(t, h)
	if len(inserts) != 1 || !strings.Contains(*decodeWriteRequest(t, inserts[0]).Event.Summary, "(return 14:00)") {
		t.Fatalf("inserts = %#v, want only the after side", inserts)
	}
}

func TestWatcherTruncatedTick(t *testing.T) {
	t.Parallel()

	meeting := wevt("m1", "Pickup", latStatic("2026-06-12T10:00:00"), latStatic("2026-06-12T11:00:00"), withLocation("200 Medical Plaza"))
	// The orphan lives on a DIFFERENT date so its only possible handling is
	// an orphan cancel, which truncation must suppress tick-wide.
	orphan := wevt("tb1", "Travel: X (for 07:00)", latStatic("2026-06-13T06:30:00"), latStatic("2026-06-13T07:00:00"))
	events := []calendarread.Event{meeting}
	for i := 0; i < 199; i++ {
		filler := wevt(fmt.Sprintf("f%d", i), "Filler", latStatic("2026-06-12T05:00:00"), latStatic("2026-06-12T05:30:00"), withTransparency("transparent"))
		events = append(events, filler)
	}
	h := newWatcherHarness(t, testNow(), map[string][]calendarread.Event{
		"2026-06-12": events,
		"2026-06-13": {orphan},
	}, nil, echoWriteSuccess(t))
	tick(t, h)

	if got := metricValue(t, h.metrics, "watch_events_truncated"); got != 1 {
		t.Fatalf("watch_events_truncated = %d, want 1", got)
	}
	if got := len(insertMessages(t, h)); got != 2 {
		t.Fatalf("inserts = %d, want 2 (inserts allowed on truncated ticks)", got)
	}
	if got := len(patchMessages(t, h)); got != 0 {
		t.Fatalf("patches = %d, want 0 (cancels suppressed on truncated ticks)", got)
	}
}

func TestWatcherContinuesAfterDateReadFailure(t *testing.T) {
	t.Parallel()

	meeting := wevt("m1", "Pickup", latStatic("2026-06-13T10:00:00"), latStatic("2026-06-13T11:00:00"), withLocation("200 Medical Plaza"))
	orphan := wevt("tb1", "Travel: X (for 15:00)", latStatic("2026-06-11T14:30:00"), latStatic("2026-06-11T15:00:00"))
	events := map[string][]calendarread.Event{
		"2026-06-11": {orphan},
		"2026-06-13": {meeting},
	}
	h := newTravelHarness(t, testNow(), func(cfg *Config) {
		cfg.WatchHorizonDays = 2
	}, func(msg upstreamMessage) (string, bool) {
		switch msg.To {
		case DefaultCalendarReadAgent:
			var req calendarread.Request
			if err := json.Unmarshal([]byte(msg.Body), &req); err != nil {
				t.Fatalf("decode read request: %v", err)
			}
			min, err := time.Parse(time.RFC3339, req.Query.TimeMin)
			if err != nil {
				t.Fatalf("parse TimeMin %q: %v", req.Query.TimeMin, err)
			}
			date := min.In(loadLocation()).Format("2006-01-02")
			if date == "2026-06-12" {
				return `{"error":"outlook calendar extraction timed out after 20s"}`, true
			}
			return calendarResponse(events[date]), true
		case DefaultCalendarWriteAgent:
			return echoWriteSuccess(t)(msg)
		default:
			return "", false
		}
	})
	tick(t, h)

	if got := metricValue(t, h.metrics, "watch_scan_date_failed"); got != 1 {
		t.Fatalf("watch_scan_date_failed = %d, want 1", got)
	}
	if got := len(insertMessages(t, h)); got != 2 {
		t.Fatalf("inserts = %d, want 2 (later successful dates still create travel)", got)
	}
	if got := len(patchMessages(t, h)); got != 0 {
		t.Fatalf("patches = %d, want 0 (degraded scan suppresses cancels)", got)
	}
}

func TestWatcherHumanRemoved(t *testing.T) {
	t.Parallel()

	meeting := wevt("m1", "Pickup", latStatic("2026-06-12T10:00:00"), latStatic("2026-06-12T11:00:00"), withLocation("200 Medical Plaza"))
	h := newWatcherHarness(t, testNow(), map[string][]calendarread.Event{"2026-06-12": {meeting}}, nil, echoWriteSuccess(t))
	tick(t, h)
	if got := len(insertMessages(t, h)); got != 2 {
		t.Fatalf("tick1 inserts = %d, want 2", got)
	}

	// Same calendar next tick: the blocks we successfully inserted are gone
	// (Joel deleted them). Denylist, do not recreate.
	h.clearUpstream()
	tick(t, h)
	if got := len(h.writeMessages()); got != 0 {
		t.Fatalf("tick2 writes = %d, want 0 (human deletion respected)", got)
	}
	if got := metricValue(t, h.metrics, "watch_travel_human_removed"); got != 2 {
		t.Fatalf("watch_travel_human_removed = %d, want 2", got)
	}

	// And it stays denylisted.
	h.clearUpstream()
	tick(t, h)
	if got := len(h.writeMessages()); got != 0 {
		t.Fatalf("tick3 writes = %d, want 0", got)
	}
}

func TestWatcherRateLimitAndBackoff(t *testing.T) {
	t.Parallel()

	day := "2026-06-12"

	t.Run("per-tick mutation cap defers overflow", func(t *testing.T) {
		t.Parallel()
		// 3 full offsite meetings (6 inserts) + 1 meeting with a blocked
		// after side (1 insert) = 7 needed mutations.
		events := []calendarread.Event{
			wevt("m1", "Visit A", latStatic("2026-06-12T08:00:00"), latStatic("2026-06-12T09:00:00"), withLocation("200 Medical Plaza")),
			wevt("m2", "Visit B", latStatic("2026-06-12T13:30:00"), latStatic("2026-06-12T14:30:00"), withLocation("Cedars-Sinai")),
			wevt("m3", "Visit C", latStatic("2026-06-12T16:00:00"), latStatic("2026-06-12T17:00:00"), withLocation("200 Medical Plaza")),
			wevt("m4", "Visit D", latStatic("2026-06-12T19:00:00"), latStatic("2026-06-12T20:00:00"), withLocation("Cedars-Sinai")),
			wevt("m5", "Dinner", latStatic("2026-06-12T20:00:00"), latStatic("2026-06-12T21:00:00")),
		}
		h := newWatcherHarness(t, testNow(), map[string][]calendarread.Event{day: events}, nil, echoWriteSuccess(t))
		tick(t, h)
		if got := len(h.writeMessages()); got != 6 {
			t.Fatalf("writes = %d, want 6 (cap)", got)
		}
		if got := metricValue(t, h.metrics, "watch_mutations_deferred"); got != 1 {
			t.Fatalf("watch_mutations_deferred = %d, want 1", got)
		}
	})

	t.Run("three consecutive failures back off the key", func(t *testing.T) {
		t.Parallel()
		meeting := wevt("m1", "Pickup", latStatic("2026-06-12T10:00:00"), latStatic("2026-06-12T11:00:00"), withLocation("200 Medical Plaza"))
		h := newWatcherHarness(t, testNow(), map[string][]calendarread.Event{day: {meeting}}, nil, func(upstreamMessage) (string, bool) {
			return writeError("rate_limited", "travel-block insert rate limit exceeded"), true
		})
		for i := 0; i < 3; i++ {
			h.clearUpstream()
			tick(t, h)
			if got := len(h.writeMessages()); got != 2 {
				t.Fatalf("tick %d writes = %d, want 2", i+1, got)
			}
		}
		if got := metricValue(t, h.metrics, "watch_travel_backoff"); got != 2 {
			t.Fatalf("watch_travel_backoff = %d, want 2 (both sides)", got)
		}
		h.clearUpstream()
		tick(t, h)
		if got := len(h.writeMessages()); got != 0 {
			t.Fatalf("post-backoff writes = %d, want 0", got)
		}
	})

	t.Run("not_owned permanently skips the event", func(t *testing.T) {
		t.Parallel()
		orphan := wevt("tb1", "Travel: X (for 10:00)", latStatic("2026-06-12T09:30:00"), latStatic("2026-06-12T10:00:00"))
		h := newWatcherHarness(t, testNow(), map[string][]calendarread.Event{day: {orphan}}, nil, func(upstreamMessage) (string, bool) {
			return writeError(ErrorNotOwned, "travel requesters may only patch their own travel blocks"), true
		})
		tick(t, h)
		if got := len(h.writeMessages()); got != 1 {
			t.Fatalf("tick1 writes = %d, want 1", got)
		}
		if got := metricValue(t, h.metrics, "watch_travel_not_owned"); got != 1 {
			t.Fatalf("watch_travel_not_owned = %d, want 1", got)
		}
		h.clearUpstream()
		tick(t, h)
		if got := len(h.writeMessages()); got != 0 {
			t.Fatalf("tick2 writes = %d, want 0 (permanent skip)", got)
		}
	})
}

func TestWatcherDisabled(t *testing.T) {
	t.Parallel()

	for _, interval := range []int{0, -5} {
		agent := NewAgent(Config{
			BusURL:           "http://127.0.0.1:0",
			AgentID:          DefaultAgentID,
			Secret:           "secret",
			WatchIntervalMin: interval,
			LocationsPath:    "../../data/locations.json",
			VenuesPath:       "../../data/venues.json",
		}, telemetry.New("scheduler-watcher-test"))
		if agent.cfg.WatchIntervalMin != 0 {
			t.Fatalf("WatchIntervalMin = %d, want clamped to 0", agent.cfg.WatchIntervalMin)
		}
		done := make(chan struct{})
		go func() {
			agent.watcher.run(context.Background())
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("watcher.run did not return for interval %d", interval)
		}
	}
}

func TestWatcherHorizonDefaultsAndOverride(t *testing.T) {
	t.Parallel()

	defaulted := NewAgent(Config{
		BusURL:        "http://127.0.0.1:0",
		AgentID:       DefaultAgentID,
		Secret:        "secret",
		LocationsPath: "../../data/locations.json",
		VenuesPath:    "../../data/venues.json",
	}, telemetry.New("scheduler-watcher-test"))
	if defaulted.cfg.WatchHorizonDays != 3 {
		t.Fatalf("default WatchHorizonDays = %d, want 3", defaulted.cfg.WatchHorizonDays)
	}

	overridden := NewAgent(Config{
		BusURL:           "http://127.0.0.1:0",
		AgentID:          DefaultAgentID,
		Secret:           "secret",
		LocationsPath:    "../../data/locations.json",
		VenuesPath:       "../../data/venues.json",
		WatchHorizonDays: 7,
	}, telemetry.New("scheduler-watcher-test"))
	if overridden.cfg.WatchHorizonDays != 7 {
		t.Fatalf("overridden WatchHorizonDays = %d, want 7", overridden.cfg.WatchHorizonDays)
	}
}

func TestWatcherIdempotentAcrossRestart(t *testing.T) {
	t.Parallel()

	day := "2026-06-12"
	events := map[string][]calendarread.Event{day: {
		wevt("m1", "Pickup", latStatic("2026-06-12T10:00:00"), latStatic("2026-06-12T11:00:00"), withLocation("200 Medical Plaza")),
	}}
	keysFor := func() []string {
		h := newWatcherHarness(t, testNow(), events, nil, echoWriteSuccess(t))
		tick(t, h)
		var keys []string
		for _, msg := range h.writeMessages() {
			keys = append(keys, metaRequestID(msg))
		}
		return keys
	}
	first := keysFor()
	second := keysFor()
	if len(first) != 2 || len(second) != 2 {
		t.Fatalf("keys = %#v / %#v, want 2 each", first, second)
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("restart changed meta.request_ids: %#v vs %#v", first, second)
		}
	}
}
