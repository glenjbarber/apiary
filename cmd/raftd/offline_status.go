package main

import (
	"encoding/json"
	"fmt"
	"os"

	raftnode "github.com/glenjbarber/apiary/internal/raft"
)

// offlineStatusJSON is the -status-json shape. It mirrors
// raftnode.OfflineStatus field for field, including the "Observed"
// booleans, so a script reading it can tell an unread value from a real
// zero exactly as the text form does - a plain uint64 term of 0 would be
// indistinguishable from "never read".
type offlineStatusJSON struct {
	NodeID  string `json:"node_id"`
	DataDir string `json:"data_dir"`

	Term         uint64 `json:"term,omitempty"`
	TermObserved bool   `json:"term_observed"`
	TermError    string `json:"term_error,omitempty"`

	FirstIndex      uint64 `json:"first_index,omitempty"`
	LastIndex       uint64 `json:"last_index,omitempty"`
	IndexesObserved bool   `json:"indexes_observed"`

	Members         []offlineMemberJSON `json:"members"`
	MembershipFrom  string              `json:"membership_source"`
	MembershipIndex uint64              `json:"membership_index,omitempty"`
	MembershipNote  string              `json:"membership_note,omitempty"`
}

type offlineMemberJSON struct {
	ID       string `json:"id"`
	Address  string `json:"address"`
	Suffrage string `json:"suffrage"`
	// IsLocal marks this node's own entry, so a reader does not have to
	// re-derive identity by comparing IDs itself.
	IsLocal bool `json:"is_local"`
}

// printOfflineStatus performs the read-only offline read and renders it.
// Rendering lives here rather than in internal/raft so that the package
// stays a pure data source with no presentation concerns.
func printOfflineStatus(cfg raftnode.Config, asJSON bool) error {
	status, err := raftnode.ReadOfflineStatus(cfg)
	if err != nil {
		return err
	}

	if asJSON {
		return renderOfflineStatusJSON(os.Stdout, status)
	}
	renderOfflineStatusText(os.Stdout, status)
	return nil
}

func renderOfflineStatusJSON(w *os.File, s raftnode.OfflineStatus) error {
	out := offlineStatusJSON{
		NodeID:          s.NodeID,
		DataDir:         s.DataDir,
		Term:            s.Term,
		TermObserved:    s.TermObserved,
		TermError:       s.TermErr,
		FirstIndex:      s.FirstIndex,
		LastIndex:       s.LastIndex,
		IndexesObserved: s.IndexesObserved,
		MembershipFrom:  s.MembershipSource,
		MembershipIndex: s.MembershipIndex,
		MembershipNote:  s.MembershipNote,
		// Non-nil so an unknown-membership read serializes as [] rather
		// than null - the difference matters to a script that ranges
		// over the result.
		Members: []offlineMemberJSON{},
	}
	for _, m := range s.Members {
		out.Members = append(out.Members, offlineMemberJSON{
			ID:       m.ID,
			Address:  m.Address,
			Suffrage: m.Suffrage,
			IsLocal:  m.ID == s.NodeID,
		})
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func renderOfflineStatusText(w *os.File, s raftnode.OfflineStatus) {
	fmt.Fprintf(w, "node:            %s\n", s.NodeID)
	fmt.Fprintf(w, "data dir:        %s\n", s.DataDir)

	if s.TermObserved {
		fmt.Fprintf(w, "term:            %d (persisted, read-only)\n", s.Term)
	} else if s.TermErr != "" {
		fmt.Fprintf(w, "term:            not observed - %s\n", s.TermErr)
	} else {
		fmt.Fprintf(w, "term:            not observed\n")
	}

	if s.IndexesObserved {
		fmt.Fprintf(w, "log bounds:      indexes %d..%d (last index includes uncommitted entries - this is not a commit index)\n",
			s.FirstIndex, s.LastIndex)
	} else {
		fmt.Fprintf(w, "log bounds:      not observed\n")
	}

	switch s.MembershipSource {
	case raftnode.MembershipFromLog:
		fmt.Fprintf(w, "membership:      %d member(s), read from the log's newest configuration entry at index %d\n",
			len(s.Members), s.MembershipIndex)
	case raftnode.MembershipFromSnapshot:
		fmt.Fprintf(w, "membership:      %d member(s), read from the latest snapshot's configuration at index %d (no newer log entry)\n",
			len(s.Members), s.MembershipIndex)
	default:
		fmt.Fprintf(w, "membership:      UNKNOWN - not empty; no configuration was found\n")
		if s.MembershipNote != "" {
			fmt.Fprintf(w, "                %s\n", s.MembershipNote)
		}
	}

	for _, m := range s.Members {
		marker := " "
		if m.ID == s.NodeID {
			marker = "*"
		}
		fmt.Fprintf(w, "  %s %-28s %-10s %s\n", marker, m.ID, m.Suffrage, m.Address)
	}

	// Stated explicitly because the absence is otherwise easy to
	// misread as an omission: leadership is genuinely not recoverable
	// here, not merely unreported.
	fmt.Fprintf(w, "\nnote:            this is an offline read of persisted state, not a live query.\n")
	fmt.Fprintf(w, "                 raft never persists its leader, so who currently holds leadership\n")
	fmt.Fprintf(w, "                 cannot be answered from disk at all - only a running raftd can say.\n")
	fmt.Fprintf(w, "                 * marks this node itself.\n")
}
