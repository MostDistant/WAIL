//go:build darwin

package main

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework Foundation
#import <Foundation/Foundation.h>

static id wailActivityToken = nil;

static void wail_disable_app_nap(void) {
	@autoreleasepool {
		if (wailActivityToken != nil) {
			return;
		}
		wailActivityToken = [[NSProcessInfo processInfo]
			beginActivityWithOptions:(NSActivityUserInitiated | NSActivityLatencyCritical)
			                  reason:@"WAIL relays realtime audio"];
	}
}
*/
import "C"

import "log"

// disableAppNap pins the process out of macOS App Nap / timer coalescing for
// the app's lifetime. WAIL opens no audio device, so once its window is
// hidden macOS throttles the whole process — the 5ms emit ticker and the Link
// Audio SDK's drain thread stall past the cushion, an audible dropout every
// time the DAW takes focus.
func disableAppNap() {
	C.wail_disable_app_nap()
	log.Println("[app] App Nap disabled (latency-critical activity)")
}
