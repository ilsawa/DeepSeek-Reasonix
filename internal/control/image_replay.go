package control

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"reasonix/internal/agent"
	"reasonix/internal/attachment"
	"reasonix/internal/i18n"
	"reasonix/internal/provider"
)

// imageReadError distinguishes local attachment loss from upload/auth failures.
// Only the former can be omitted when replaying an earlier user turn.
type imageReadError struct{ error }

func (e imageReadError) Unwrap() error { return e.error }

func lastImageRequestTurn(messages []provider.Message) int {
	for i, m := range slices.Backward(messages) {
		if m.Role == provider.RoleUser && !m.LocalOnly && !agent.IsHostGeneratedUserMessage(m) {
			return i
		}
	}
	return 0 // no known user boundary: keep all inputs required
}

// answeredImageLookOrder is the host sentence that tells a vision model to
// read an attached image. It stays on the session message. A later request
// must not repeat it: the pixels are no longer attached.
const answeredImageLookOrder = "Look at the image directly; do not OCR or read the file unless the user asks."

const answeredImageWithheldNote = "[This earlier image was already shown and is not attached again. Use the earlier description in the conversation. Do not inspect the image again unless the user attaches it once more.]"

var answeredImageNoteRE = regexp.MustCompile(`\[(?:The image at @|image file )[^\[\]]*` + regexp.QuoteMeta(answeredImageLookOrder) + `\]\n?`)

var emptyImageTagRE = regexp.MustCompile(`(?s)<image\b[^>]*>\s*</image>\n?`)

func messageCarriesImage(m provider.Message) bool {
	return len(m.Images) > 0 || len(m.ImageInputs) > 0 || strings.Contains(m.Content, answeredImageLookOrder)
}

// withholdAnsweredImages drops an already answered image from one request
// copy. The session message keeps its attachment and the original wording.
func withholdAnsweredImages(m *provider.Message) {
	m.Images = nil
	m.ImageInputs = nil
	if strings.Contains(m.Content, answeredImageLookOrder) {
		m.Content = answeredImageNoteRE.ReplaceAllString(m.Content, "")
		m.Content = emptyImageTagRE.ReplaceAllString(m.Content, "")
	}
	if strings.Contains(m.Content, "not attached again") {
		return
	}
	if strings.TrimSpace(m.Content) != "" && !strings.HasSuffix(m.Content, "\n") {
		m.Content += "\n"
	}
	m.Content += answeredImageWithheldNote
}

// Resolve independently so a missing old attachment does not discard its
// healthy siblings. The durable references and UI transcript remain intact.
func (c *Controller) resolveReplayImages(ctx context.Context, inputs []attachment.ImageInput, route ImageRequestRoute, historical bool) ([]string, []int, error) {
	var images []string
	var missing []int
	for i, input := range inputs {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		resolved, err := c.resolveImageInputsForRoute(ctx, []attachment.ImageInput{input}, route)
		if err == nil {
			images = append(images, resolved...)
			continue
		}
		if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, nil, err
		}
		var readErr imageReadError
		if historical && (errors.As(err, &readErr) || input.Validate() != nil) {
			missing = append(missing, i+1)
			continue
		}
		var item attachment.Error
		if errors.As(err, &item) {
			item.Index = i + 1
			return nil, nil, item
		}
		return nil, nil, err
	}
	return images, missing, nil
}

func noteUnavailableImages(m *provider.Message, positions []int) {
	if len(positions) == 0 {
		return
	}
	m.Content += fmt.Sprintf("\n[Historical image inputs %v are unavailable. Their contents have not been supplied in this request. Continue with available text and images; if those missing images are needed, ask the user to attach them again. Do not infer their contents.]", positions)
}

func noteUnreadableImages(m *provider.Message, count int) {
	positions := make([]int, count)
	for i := range positions {
		positions[i] = i + 1
	}
	m.Content += fmt.Sprintf("\n[Image inputs %v were not supplied: the selected model cannot read images and no image understanding model is configured. Do not infer their contents; tell the user to pick a model that reads images or set an image understanding model.]", positions)
}

func imageRequestFailure(ctx context.Context, err error) error {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%s: %w", i18n.M.ImageRequestRecovery, err)
}
