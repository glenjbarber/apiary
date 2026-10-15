package frontend

import (
	"net/http"

)

// This file is the HTTP half of the guided join flow: the wizard page
// (web/templates/join_guided.html) that guides the operator through
// joining a new Comb to an existing Colony.
//
// The flow consists of five steps:
//
//   1. Generate first code & fingerprints on the joining Comb.
//   2. Enter the first code & fingerprints on the target Colony's Admin UI.
//   3. Prepare the joining Comb (wipe raft state, restart with -await-join).
//   4. Poll for the second PIN on the joining Comb.
//   5. Enter the second PIN & confirmation phrase on the target Colony's Admin UI.
//
// Each step is validated (where possible) before proceeding to the next.
// The wizard uses HTMX for step-by-step rendering and server-side state
// tracking via the Guided struct in the page data.
//
// Compatibility: This is a new feature, so there are no existing endpoints
// to preserve. We introduce a new handler at /colony/join/guided.

// handleJoinGuidedPage serves the wizard at /colony/join/guided.
// GET: renders the wizard at the appropriate step based on submitted data.
// POST: processes the current step's data and advances to the next step.
func (s *Server) handleJoinGuidedPage(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.renderJoinGuidedPage(w, r, nil)
	case http.MethodPost:
		s.processJoinGuidedStep(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// renderJoinGuidedPage renders the wizard with the current step determined
// from the provided joinGuidedData (which may be nil for the initial step).
func (s *Server) renderJoinGuidedPage(w http.ResponseWriter, r *http.Request, data *joinGuidedData) {
	if data == nil {
		data = &joinGuidedData{}
	}

	// Determine the current step from the data.
	step := data.Step
	if step == "" {
		step = "generate"
	}

	// Build the Guided struct for the template.
	guided := &guidedJoinPageView{
		Step:   step,
		Action: "/colony/join/guided",
		// We'll store the submitted data in the Guided struct so it's available
		// in the template for display (e.g., to show the second PIN when polled).
		SecondPin: data.SecondPin,
	}

	s.render(w, "join_guided_page", s.withAuthFieldsFrom(r, pageData{
		Guided: guided,
	}, nil, nil))
}

// processJoinGuidedStep handles the POST request for each step.
// It validates the current step's data (if applicable) and then
// determines the next step, storing any necessary data (like the
// second PIN when polled) in the joinGuidedData for the next render.
func (s *Server) processJoinGuidedStep(w http.ResponseWriter, r *http.Request) {
	// Parse the form data.
	if err := r.ParseForm(); err != nil {
		s.renderJoinGuidedPage(w, r, &joinGuidedData{
			Step:   "error",
			Error:  "Failed to parse form: " + err.Error(),
		})
		return
	}

	// Determine the current step from the submitted step parameter.
	currentStep := r.FormValue("step")
	if currentStep == "" {
		// If no step is submitted, we assume we are at the first step.
		currentStep = "generate"
	}

	// We'll store data that needs to persist across steps in this struct.
	data := &joinGuidedData{
		Step: currentStep,
	}

	// Process each step.
	switch currentStep {
	case "generate":
		// Step 1: Generate first code & fingerprints.
		// In this step, the operator runs a command on the joining Comb.
		// We don't have any data to validate from the operator yet.
		// We just advance to the next step.
		data.Step = "first-code-entered"

	case "first-code-entered":
		// Step 2: Enter first code & fingerprints.
		// We validate that the first code and fingerprints are provided.
		firstCode := r.FormValue("first_code")
		fingerprints := r.FormValue("fingerprints")
		if firstCode == "" || fingerprints == "" {
			data.Error = "First code and fingerprints are required"
			data.Step = currentStep // Stay on the same step
			data.FirstCode = firstCode
			data.Fingerprints = fingerprints
			s.renderJoinGuidedPage(w, r, data)
			return
		}
		// In a real implementation, we would validate the first code and
		// fingerprints against the joining Comb's output. For now, we just
		// store them and advance.
		data.FirstCode = firstCode
		data.Fingerprints = fingerprints
		data.Step = "prepared"

	case "prepare":
		// Step 3: Prepare joining Comb.
		// The operator runs a command on the joining Comb.
		// No data to validate from the operator.
		data.Step = "second-pin-polled"
		// In a real implementation, we would now start polling the joining
		// Comb for the second PIN. For this prototype, we'll simulate by
		// setting a dummy second PIN after a short delay? But we cannot
		// do that in the handler. Instead, we'll rely on the template to
		// poll via JavaScript (see the template's pollSecondPin function).
		// We'll leave SecondPin empty in the data, and the template will
		// show "Waiting for second PIN..." and then reload until it appears.
		// For now, we'll set a placeholder to indicate that polling has started.
		// In a real implementation, we would store the join request ID and
		// then poll the managerd for the second PIN.
		// We'll leave it empty and let the template handle the polling.

	case "second-pin-polled":
		// Step 4: Poll for second PIN.
		// In this step, we expect that the second PIN has been polled and
		// is available. In the template, we show the PIN if it's set.
		// We'll get the second PIN from the form? Actually, the template
		// does not submit the second PIN in this step; it only has a button
		// to say "I have the second PIN". So we need to get the second PIN
		// from somewhere else.
		// For now, we'll assume that the second PIN has been stored in the
		// data from a previous poll (maybe by a background process). Since
		// we don't have that, we'll leave it empty and let the template
		// show the waiting message.
		// We'll advance to the next step when the operator clicks the button.
		data.Step = "second-code-entered"

	case "second-code-entered":
		// Step 5: Enter second code & confirmation phrase.
		secondPin := r.FormValue("second_pin")
		confirmation := r.FormValue("confirmation_phrase")
		if secondPin == "" || confirmation == "" {
			data.Error = "Second PIN and confirmation phrase are required"
			data.Step = currentStep
			data.SecondPin = secondPin
			data.ConfirmationPhrase = confirmation
			s.renderJoinGuidedPage(w, r, data)
			return
		}
		// Validate the second PIN and confirmation phrase against the
		// joining Comb's output (in a real implementation).
		data.SecondPin = secondPin
		data.ConfirmationPhrase = confirmation
		// After successful validation, we would complete the join.
		// For now, we just mark the step as complete.
		data.Step = "complete"

	default:
		data.Error = "Unknown step: " + currentStep
		data.Step = currentStep
	}

	// If we have an error, we render the page with the error and stay on the same step.
	if data.Error != "" {
		s.renderJoinGuidedPage(w, r, data)
		return
	}

	// If we are not at an error and we have advanced the step, we render the page for the new step.
	// But note: we already set data.Step to the next step above, so we just render.
	s.renderJoinGuidedPage(w, r, data)
}

// joinGuidedData holds the data that flows between the handler and the template
// for the guided join wizard. It is stored in the template's Guided field.
type joinGuidedData struct {
	// Step is the current step identifier (generate, first-code-entered, prepared,
	// second-pin-polled, second-code-entered, complete, error).
	Step string

	// Error is an optional error message to display in the banner.
	Error string

	// Data from step 2: first code and fingerprints.
	FirstCode     string
	Fingerprints  string

	// Data from step 4: second PIN (polled from the joining Comb).
	SecondPin string

	// Data from step 5: confirmation phrase.
	ConfirmationPhrase string
}

// guidedJoinPageView is the wizard's own data for the template.
type guidedJoinPageView struct {
	// Step is the current step.
	Step string
	// Action is the form submission URL.
	Action string
	// SecondPin is the second PIN to display in step 4 (if available).
	SecondPin string
}

// validateJoinGuidedStep is a placeholder for any validation that might be
// needed for each step. Currently, we do validation inline in the step
// processing above.
func (s *Server) validateJoinGuidedStep(r *http.Request, step string) error {
	// TODO: Implement validation for each step.
	return nil
}
