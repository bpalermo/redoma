// Package rolesanywhere exchanges an agent's X.509 SVID for AWS session
// credentials through IAM Roles Anywhere's CreateSession: a request signed
// as Signature Version 4 is, but with the certificate's private key in place
// of a secret access key (AWS4-X509-ECDSA-SHA256 or AWS4-X509-RSA-SHA256),
// the certificate and its intermediates in headers.
//
// The certificate and the key come from memory, as redoma xds holds them
// (identity.X509SVID); nothing is read from or written to a file.
package rolesanywhere

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Session durations Roles Anywhere accepts (durationSeconds), and the one a
// Profile without its own asks for.
const (
	MinDuration     = 15 * time.Minute
	MaxDuration     = 12 * time.Hour
	DefaultDuration = time.Hour
)

// Profile is what a session is asked for: the trust anchor that vouches for
// the certificate, the profile that maps it to roles, the role, and how long
// the session lasts.
type Profile struct {
	TrustAnchorARN string
	ProfileARN     string
	RoleARN        string
	// Duration is the session's; DefaultDuration when zero.
	Duration time.Duration
}

var (
	// arn:<partition>:rolesanywhere:<region>:<account>:<kind>/<uuid>
	rolesAnywhereARN = regexp.MustCompile(`^arn:(aws|aws-cn|aws-us-gov):rolesanywhere:([a-z]{2}(?:-gov)?-[a-z]+-[0-9]):([0-9]{12}):(trust-anchor|profile)/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`)
	// arn:<partition>:iam::<account>:role/<path/><name>
	roleARN = regexp.MustCompile(`^arn:(aws|aws-cn|aws-us-gov):iam::([0-9]{12}):role/(?:[\w+=,.@-]+/)*[\w+=,.@-]{1,64}$`)
)

// parsed is the fields of a Roles Anywhere ARN.
type parsed struct {
	partition, region, account, kind string
}

func parseRolesAnywhere(arn, kind string) (parsed, error) {
	m := rolesAnywhereARN.FindStringSubmatch(arn)
	if m == nil || m[4] != kind {
		return parsed{}, fmt.Errorf("%q is not a Roles Anywhere %s ARN (arn:<partition>:rolesanywhere:<region>:<account>:%s/<id>)", arn, kind, kind)
	}
	return parsed{partition: m[1], region: m[2], account: m[3], kind: m[4]}, nil
}

// Validate checks the three ARNs and the duration: each ARN in its exact
// form; the trust anchor's region one of its partition's; the trust anchor
// and the profile in one partition, region and account, the role in that partition and account (Roles Anywhere maps a
// certificate to roles of its own account only); the duration in whole
// seconds between MinDuration and MaxDuration.
func (p Profile) Validate() error {
	anchor, err := parseRolesAnywhere(p.TrustAnchorARN, "trust-anchor")
	if err != nil {
		return err
	}
	profile, err := parseRolesAnywhere(p.ProfileARN, "profile")
	if err != nil {
		return err
	}
	if partitionOf(anchor.region) != anchor.partition {
		return fmt.Errorf("trust anchor %q: region %s is not in partition %s", p.TrustAnchorARN, anchor.region, anchor.partition)
	}
	if profile.partition != anchor.partition || profile.region != anchor.region || profile.account != anchor.account {
		return fmt.Errorf("profile %q is not in the trust anchor's partition, region and account (%s, %s, %s)", p.ProfileARN, anchor.partition, anchor.region, anchor.account)
	}
	m := roleARN.FindStringSubmatch(p.RoleARN)
	if m == nil {
		return fmt.Errorf("%q is not an IAM role ARN (arn:<partition>:iam::<account>:role/<name>)", p.RoleARN)
	}
	if m[1] != anchor.partition || m[2] != anchor.account {
		return fmt.Errorf("role %q is not in the trust anchor's partition and account (%s, %s)", p.RoleARN, anchor.partition, anchor.account)
	}
	d := p.duration()
	if d < MinDuration || d > MaxDuration || d%time.Second != 0 {
		return fmt.Errorf("session duration %s is not whole seconds between %s and %s", d, MinDuration, MaxDuration)
	}
	return nil
}

// partitionOf is the partition a region is in: China's regions (cn-) are
// aws-cn's, GovCloud's (us-gov-) aws-us-gov's, every other one aws's.
func partitionOf(region string) string {
	switch {
	case strings.HasPrefix(region, "cn-"):
		return "aws-cn"
	case strings.HasPrefix(region, "us-gov-"):
		return "aws-us-gov"
	default:
		return "aws"
	}
}

func (p Profile) duration() time.Duration {
	if p.Duration == 0 {
		return DefaultDuration
	}
	return p.Duration
}

// Region is the trust anchor's region, where its sessions are asked for.
// Empty when the ARN is not a trust anchor's.
func (p Profile) Region() string {
	a, err := parseRolesAnywhere(p.TrustAnchorARN, "trust-anchor")
	if err != nil {
		return ""
	}
	return a.region
}

// Host is the Roles Anywhere endpoint of the trust anchor's region and
// partition: rolesanywhere.<region>.amazonaws.com, or .amazonaws.com.cn in
// China. Empty when the ARN is not a trust anchor's.
func (p Profile) Host() string {
	a, err := parseRolesAnywhere(p.TrustAnchorARN, "trust-anchor")
	if err != nil {
		return ""
	}
	return Host(a.partition, a.region)
}

// Host is the Roles Anywhere endpoint of a partition's region.
func Host(partition, region string) string {
	h := "rolesanywhere." + region + ".amazonaws.com"
	if strings.HasPrefix(partition, "aws-cn") {
		h += ".cn"
	}
	return h
}
