//go:build darwin

package main

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework AppKit
#include <stdlib.h>
#import <AppKit/AppKit.h>

// pkPasteboardPNG copies the general pasteboard's image as PNG bytes.
//
// It prefers a PNG the pasteboard already holds, which is what a screenshot copied with
// Ctrl+Shift+Cmd+4 puts there, and falls back to TIFF, which is all that an image copied
// from Preview or a browser may offer. It returns NULL, with *len 0, when the pasteboard
// holds no image. The caller frees the buffer.
static void *pkPasteboardPNG(long *changeCount, size_t *len) {
	@autoreleasepool {
		NSPasteboard *pb = [NSPasteboard generalPasteboard];
		*changeCount = (long)[pb changeCount];
		*len = 0;

		NSData *png = [pb dataForType:NSPasteboardTypePNG];
		if (png == nil) {
			NSData *tiff = [pb dataForType:NSPasteboardTypeTIFF];
			if (tiff == nil) {
				return NULL;
			}
			NSBitmapImageRep *rep = [NSBitmapImageRep imageRepWithData:tiff];
			if (rep == nil) {
				return NULL;
			}
			png = [rep representationUsingType:NSBitmapImageFileTypePNG properties:@{}];
			if (png == nil) {
				return NULL;
			}
		}
		void *buf = malloc([png length]);
		if (buf == NULL) {
			return NULL;
		}
		memcpy(buf, [png bytes], [png length]);
		*len = [png length];
		return buf;
	}
}

static long pkPasteboardChangeCount(void) {
	@autoreleasepool {
		return (long)[[NSPasteboard generalPasteboard] changeCount];
	}
}
*/
import "C"

// macPasteboard reads the Mac's general pasteboard in process. The daemon's launchd job
// runs in the Aqua session (LimitLoadToSessionType), which is what gives it a pasteboard
// to read at all.
type macPasteboard struct{}

func (macPasteboard) changeCount() int64 { return int64(C.pkPasteboardChangeCount()) }

func (macPasteboard) imagePNG() ([]byte, int64) {
	var cc C.long
	var n C.size_t
	p := C.pkPasteboardPNG(&cc, &n)
	if p == nil {
		return nil, int64(cc)
	}
	defer C.free(p)
	return C.GoBytes(p, C.int(n)), int64(cc)
}

func systemPasteboard() pasteboard { return macPasteboard{} }
