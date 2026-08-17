package detect

import (
	"fmt"
	"strings"

	"github.com/backendArchitect/argus/internal/model"
)

// pullCause classifies why an image pull failed. All four surface identically as
// ImagePullBackOff and have completely different fixes, which is exactly why
// "ImagePullBackOff" on its own is a symptom and not a diagnosis.
type pullCause struct {
	id      string
	title   string
	detail  string
	markers []string // lowercase substrings of the runtime's error message
}

// Ordered most-specific first: an auth failure message can also contain the word
// "not found", so the narrower markers must win.
var pullCauses = []pullCause{
	{
		id:      "image.pull-rate-limited",
		title:   "Image pull is being rate limited by the registry",
		detail:  "The registry is refusing pulls because a rate limit has been hit, most often Docker Hub's anonymous limit. Authenticating the node or mirroring the image into your own registry fixes this; retrying does not.",
		markers: []string{"toomanyrequests", "rate limit", "429"},
	},
	{
		id:      "image.pull-unauthorized",
		title:   "Image pull was rejected: the registry refused the credentials",
		detail:  "The registry answered but rejected the request. Either no imagePullSecret is attached to the pod's service account, the secret does not cover this registry, or its credentials have expired. Note this is distinct from a missing tag — the image may well exist.",
		markers: []string{"unauthorized", "authentication required", "denied", "forbidden", "401", "403"},
	},
	{
		id:      "image.pull-not-found",
		title:   "Image pull failed: the tag does not exist in the registry",
		detail:  "The registry answered and reported no such image or tag. This is nearly always a typo in the tag, or a CI pipeline that has not finished pushing the image the manifest already references.",
		markers: []string{"not found", "manifest unknown", "manifest for", "no such host", "does not exist"},
	},
}

// detectImagePull finds containers stuck unable to pull their image, and says
// which of the four failure modes it is.
func detectImagePull(s *model.Snapshot) []model.Finding {
	for i := range s.Pods {
		pod := &s.Pods[i]
		for j := range pod.Containers {
			c := &pod.Containers[j]
			if c.State.Status != "waiting" {
				continue
			}
			if c.State.Reason != "ImagePullBackOff" && c.State.Reason != "ErrImagePull" {
				continue
			}

			// The pod status message is often truncated; the events carry the full
			// error from the runtime, so search both.
			haystack := strings.ToLower(c.State.Message + " " + pullEventText(s))
			cause := classifyPull(haystack)

			ev := []model.Evidence{
				evidence("pod.status", "pod/"+pod.Name,
					"container %q is %s: %s", c.Name, c.State.Reason, firstLine(c.State.Message)),
				evidence("pod.spec", "pod/"+pod.Name, "image is %q", c.Image),
			}
			// Prefer the event carrying the registry's own words. The generic
			// "Error: ImagePullBackOff" line is always present and says nothing; the
			// useful one names the actual failure, and it is what classification read.
			if g := bestPullEvent(s); g != nil {
				ev = append(ev, evidence("event", g.ObjectKind+"/"+g.ObjectName,
					"%s x%d across %d pod(s): %s", g.Reason, g.Count, g.ObjectCount, g.Message))
			}

			conf := 0.9
			if cause.id == "image.pull-failed" {
				// Unclassified: we know the pull failed but not why, so claim less.
				conf = 0.6
			}

			return []model.Finding{{
				ID:         cause.id,
				Severity:   model.Critical,
				Confidence: confidence(s, conf, "events"),
				Scope:      workloadScope(s),
				Title:      cause.title,
				Detail: fmt.Sprintf("%s Container %q of %s cannot start, so the pod will never "+
					"become ready.", cause.detail, c.Name, pod.Name),
				Evidence: ev,
			}}
		}
	}
	return nil
}

func classifyPull(haystack string) pullCause {
	for _, c := range pullCauses {
		for _, m := range c.markers {
			if strings.Contains(haystack, m) {
				return c
			}
		}
	}
	// Honest fallback: the pull failed, we could not tell why. Better than
	// guessing "typo" and sending someone to check a tag that is perfectly fine.
	return pullCause{
		id:    "image.pull-failed",
		title: "Image pull is failing for an unrecognised reason",
		detail: "The container runtime could not pull the image, but the error did not match a " +
			"known cause (missing tag, credentials, or rate limit). The raw message is in the " +
			"evidence below.",
	}
}

// pullEventText gathers image-related event messages so classification can read
// the full runtime error rather than the truncated pod status.
func pullEventText(s *model.Snapshot) string {
	var b strings.Builder
	for _, g := range s.Events {
		if g.Reason == "Failed" || g.Reason == "BackOff" || g.Reason == "Pulling" {
			b.WriteString(g.Message)
			b.WriteByte(' ')
		}
	}
	return b.String()
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}

// bestPullEvent picks the image-related event with the most diagnostic content:
// one whose message matched a known cause, else the longest, since the runtime's
// real error is verbose and the generic backoff line is short.
func bestPullEvent(s *model.Snapshot) *model.EventGroup {
	var best *model.EventGroup
	for i := range s.Events {
		g := &s.Events[i]
		if g.Type != "Warning" || !strings.Contains(strings.ToLower(g.Message), "image") {
			continue
		}
		lower := strings.ToLower(g.Message)
		scored := false
		for _, c := range pullCauses {
			for _, m := range c.markers {
				if strings.Contains(lower, m) {
					scored = true
				}
			}
		}
		if scored {
			return g
		}
		if best == nil || len(g.Message) > len(best.Message) {
			best = g
		}
	}
	return best
}
