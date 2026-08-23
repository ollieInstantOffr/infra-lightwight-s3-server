package db

import "fmt"

// VersioningState is a bucket's versioning setting.
//
// Three states rather than a boolean, because S3 has three and the difference
// between two of them is visible to clients. A bucket that has never been
// versioned omits the Status element from GetBucketVersioning entirely, which
// is how a client tells "never turned on" from "turned on and then off".
type VersioningState string

const (
	// VersioningUnversioned is a bucket versioning was never enabled on. Its
	// objects have the null version id.
	VersioningUnversioned VersioningState = "unversioned"
	// VersioningEnabled keeps every version.
	VersioningEnabled VersioningState = "enabled"
	// VersioningSuspended stops creating versions without discarding the ones
	// already there. New writes take the null version id, and overwriting the
	// null version replaces it rather than keeping both — which is the one
	// genuinely surprising rule in S3 versioning, and the reason suspending is
	// not a safe way to pause history.
	VersioningSuspended VersioningState = "suspended"
)

// Versioned reports whether writes should create new versions.
func (v VersioningState) Versioned() bool { return v == VersioningEnabled }

// Configured reports whether versioning has ever been turned on. Once it has,
// S3 never lets a bucket return to the unversioned state.
func (v VersioningState) Configured() bool { return v != VersioningUnversioned }

// Valid reports whether v is a state this server understands.
func (v VersioningState) Valid() bool {
	switch v {
	case VersioningUnversioned, VersioningEnabled, VersioningSuspended:
		return true
	}
	return false
}

// TransitionTo validates a requested change and returns the new state.
//
// S3 allows Enabled and Suspended in a PutBucketVersioning request, and nothing
// else — there is no way to return a bucket to the unversioned state once
// versioning has been turned on. Refusing that explicitly is kinder than
// accepting it and quietly doing something else, which would leave version ids
// already handed out pointing at a bucket that claims never to have had any.
func (v VersioningState) TransitionTo(next VersioningState) (VersioningState, error) {
	switch next {
	case VersioningEnabled, VersioningSuspended:
		return next, nil
	case VersioningUnversioned:
		if v.Configured() {
			return v, fmt.Errorf(
				"versioning cannot be turned off once it has been enabled; suspend it instead")
		}
		return v, nil
	default:
		return v, fmt.Errorf("%q is not a versioning state", next)
	}
}

// NullVersionID is the id S3 reports for an object written while its bucket was
// not versioned. It is a real, addressable id: a client may GET or DELETE
// ?versionId=null, and expects it to work.
const NullVersionID = "null"

// externalVersionID renders a stored version id for a client. The empty string
// is how the null version is stored, since an object written before versioning
// existed genuinely has no identifier of its own.
func externalVersionID(stored string) string {
	if stored == "" {
		return NullVersionID
	}
	return stored
}

// internalVersionID converts a client-supplied version id back to storage form.
func internalVersionID(external string) string {
	if external == NullVersionID {
		return ""
	}
	return external
}
