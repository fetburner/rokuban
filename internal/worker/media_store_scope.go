package worker

// mediaScopeIsDerivedObjectStore は worker の media store scope が派生物の置き場と同じかを返す。
func mediaScopeIsDerivedObjectStore(mediaStoreName, derivedObjectStoreName string) bool {
	return mediaStoreName == derivedObjectStoreName
}
