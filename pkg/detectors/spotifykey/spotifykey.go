package spotifykey

import (
	"context"

	"regexp"
	"strings"

	"github.com/trufflesecurity/trufflehog/pkg/detectors"

	"golang.org/x/oauth2/clientcredentials"

	"github.com/trufflesecurity/trufflehog/pkg/pb/detectorspb"
)

type Scanner struct{}

// Ensure the Scanner satisfies the interface at compile time
var _ detectors.Detector = (*Scanner)(nil)

var (
	//Make sure that your group is surrounded in boundry characters such as below to reduce false positives
	secretPat = regexp.MustCompile(detectors.PrefixRegex([]string{"key", "secret"}) + `\b([A-Za-z0-9]{32})\b`)
	idPat     = regexp.MustCompile(detectors.PrefixRegex([]string{"id"}) + `\b([A-Za-z0-9]{32})\b`)
)

// Keywords are used for efficiently pre-filtering chunks.
// Use identifiers in the secret preferably, or the provider name.
func (s Scanner) Keywords() []string {
	return []string{"spotify"}
}

// FromData will find and optionally verify SpotifyKey secrets in a given set of bytes.
func (s Scanner) FromData(ctx context.Context, verify bool, data []byte) (results []detectors.Result, err error) {
	dataStr := string(data)

	matches := secretPat.FindAllStringSubmatch(dataStr, -1)
	idMatches := idPat.FindAllStringSubmatch(dataStr, -1)

	for _, match := range matches {
		if len(match) != 2 {
			continue
		}
		resMatch := strings.TrimSpace(match[1])
		for _, idMatch := range idMatches {
			if len(idMatch) != 2 {
				continue
			}
			idresMatch := strings.TrimSpace(idMatch[1])
			s1 := detectors.Result{
				DetectorType: detectorspb.DetectorType_SpotifyKey,
				Raw:          []byte(resMatch),
			}

			if verify {
				config := &clientcredentials.Config{
					ClientID:     idresMatch,
					ClientSecret: resMatch,
					TokenURL:     "https://accounts.spotify.com/api/token",
				}
				token, err := config.Token(context.Background())
				if err == nil {
					if token.Type() == "Bearer" {
						s1.Verified = true
					}
				}
			}

			results = append(results, s1)
		}

	}

	return detectors.CleanResults(results), nil
}