package wire

// RefreshMarker is appended to the matched key of a stale exact hit on a
// refresh-eligible key so the client re-saves the cache at the end of the job
// instead of skipping the save.
const RefreshMarker = "~refresh"
