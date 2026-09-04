#import <AppKit/AppKit.h>
#import <ApplicationServices/ApplicationServices.h>
#import <AVFoundation/AVFoundation.h>
#import <AudioToolbox/AudioToolbox.h>
#import <CoreAudio/CoreAudio.h>
#import <CoreGraphics/CoreGraphics.h>
#import <ImageIO/ImageIO.h>
#import <IOKit/hidsystem/IOHIDLib.h>
#import <ScreenCaptureKit/ScreenCaptureKit.h>
#import <Speech/Speech.h>
#import <Vision/Vision.h>

#include <os/lock.h>
#include <stdlib.h>
#include <string.h>
#include <stdbool.h>
#include <unistd.h>

static char *LumiCopyUTF8(NSString *value) {
    if (value == nil) {
        return NULL;
    }
    const char *utf8 = value.UTF8String;
    return utf8 == NULL ? NULL : strdup(utf8);
}

void lumi_os_version(int *major, int *minor, int *patch) {
    NSOperatingSystemVersion version = NSProcessInfo.processInfo.operatingSystemVersion;
    if (major != NULL) *major = (int)version.majorVersion;
    if (minor != NULL) *minor = (int)version.minorVersion;
    if (patch != NULL) *patch = (int)version.patchVersion;
}

static char *LumiCopyError(NSError *error) {
    return LumiCopyUTF8(error.localizedDescription ?: @"unknown native macOS error");
}

static NSError *LumiTimeoutError(NSString *operation) {
    return [NSError errorWithDomain:@"LumiNative" code:3
                           userInfo:@{NSLocalizedDescriptionKey:
                                          [NSString stringWithFormat:@"%@ timed out", operation]}];
}

static BOOL LumiWait(dispatch_semaphore_t semaphore, NSTimeInterval seconds,
                     NSString *operation, NSError **error) {
    int64_t nanoseconds = (int64_t)(MAX(0.1, seconds) * (double)NSEC_PER_SEC);
    if (dispatch_semaphore_wait(semaphore, dispatch_time(DISPATCH_TIME_NOW, nanoseconds)) == 0) {
        return YES;
    }
    if (error != NULL) *error = LumiTimeoutError(operation);
    return NO;
}

static NSString *LumiJSONString(id object, NSError **error) {
    NSData *data = [NSJSONSerialization dataWithJSONObject:object options:0 error:error];
    return data == nil ? nil : [[NSString alloc] initWithData:data encoding:NSUTF8StringEncoding];
}

static BOOL LumiWriteJPEG(CGImageRef image, NSString *path, NSError **error) {
    NSURL *url = [NSURL fileURLWithPath:path];
    CGImageDestinationRef destination = CGImageDestinationCreateWithURL(
        (__bridge CFURLRef)url, CFSTR("public.jpeg"), 1, NULL);
    if (destination == NULL) {
        if (error != NULL) {
            *error = [NSError errorWithDomain:@"LumiNative" code:1
                                     userInfo:@{NSLocalizedDescriptionKey: @"create JPEG destination"}];
        }
        return NO;
    }
    NSDictionary *properties = @{(__bridge NSString *)kCGImageDestinationLossyCompressionQuality: @0.82};
    CGImageDestinationAddImage(destination, image, (__bridge CFDictionaryRef)properties);
    BOOL finalized = CGImageDestinationFinalize(destination);
    CFRelease(destination);
    if (!finalized && error != NULL) {
        *error = [NSError errorWithDomain:@"LumiNative" code:2
                                 userInfo:@{NSLocalizedDescriptionKey: @"finalize JPEG image"}];
    }
    return finalized;
}

// LumiParseDisplayAllowlist turns a caller's comma-separated display IDs into a
// set. An absent or empty list means "every display" and yields nil, which every
// caller reads as "no filter" rather than as "nothing is allowed".
static NSSet<NSNumber *> *LumiParseDisplayAllowlist(const char *csv) {
    if (csv == NULL) return nil;
    NSCharacterSet *blank = NSCharacterSet.whitespaceAndNewlineCharacterSet;
    NSString *text = [[NSString stringWithUTF8String:csv] stringByTrimmingCharactersInSet:blank];
    if (text.length == 0) return nil;
    NSMutableSet<NSNumber *> *ids = [NSMutableSet set];
    for (NSString *piece in [text componentsSeparatedByString:@","]) {
        NSString *trimmed = [piece stringByTrimmingCharactersInSet:blank];
        if (trimmed.length == 0) continue;
        [ids addObject:@((uint32_t)strtoul(trimmed.UTF8String, NULL, 10))];
    }
    return ids.count == 0 ? nil : ids;
}

// LumiSelectDisplays decides which of the displays that are actually present
// should be captured, and reports whether the caller's selection was ignored.
//
// It resolves the selection against the very list the capture loop iterates,
// never against a second enumeration. A display that one API lists and the other
// does not — asleep, mirrored, mid-transition — would otherwise make an
// allowlist look satisfiable and then match nothing, capturing nothing at all.
//
// An allowlist naming no connected display therefore falls back to every
// display, which is recoverable; capturing nothing is not. A deselected screen
// being recorded is invisible in the captured data, so the caller is handed
// `fallback` and is expected to say so out loud.
static NSArray<NSNumber *> *LumiSelectDisplays(NSArray<NSNumber *> *present,
                                               NSSet<NSNumber *> *allowed, BOOL *fallback) {
    if (fallback != NULL) *fallback = NO;
    if (allowed == nil) return present;
    NSMutableArray<NSNumber *> *chosen = [NSMutableArray arrayWithCapacity:present.count];
    for (NSNumber *displayID in present) {
        if ([allowed containsObject:displayID]) [chosen addObject:displayID];
    }
    if (chosen.count > 0) return chosen;
    if (fallback != NULL) *fallback = YES;
    return present;
}

// lumi_select_displays_json drives the selection rule directly, so which
// displays an allowlist chooses — and when it is ignored — is testable without a
// live capture session. It exists for the test, like lumi_resolve_frontmost_json:
// asserting the rule through a real capture would pass vacuously on the
// single-display machine most tests run on.
char *lumi_select_displays_json(const char *display_ids_json, const char *allowlist_csv,
                                char **error_message) {
    @autoreleasepool {
        NSString *text = display_ids_json == NULL
            ? @"[]" : [NSString stringWithUTF8String:display_ids_json];
        NSError *error = nil;
        id parsed = [NSJSONSerialization JSONObjectWithData:[text dataUsingEncoding:NSUTF8StringEncoding]
                                                    options:0
                                                      error:&error];
        if (![parsed isKindOfClass:NSArray.class]) {
            if (error_message != NULL) {
                *error_message = LumiCopyUTF8(error.localizedDescription ?: @"decode display id list");
            }
            return NULL;
        }
        BOOL fallback = NO;
        NSArray<NSNumber *> *chosen =
            LumiSelectDisplays(parsed, LumiParseDisplayAllowlist(allowlist_csv), &fallback);
        NSString *json = LumiJSONString(@{@"display_ids": chosen,
                                          @"selection_fallback": @(fallback)}, &error);
        if (json == nil) {
            if (error_message != NULL) *error_message = LumiCopyError(error);
            return NULL;
        }
        return LumiCopyUTF8(json);
    }
}

char *lumi_capture_screens_json(const char *directory, const char *prefix,
                                const char *display_ids_csv, int max_pixel_width,
                                char **error_message) {
    @autoreleasepool {
        __block SCShareableContent *content = nil;
        __block NSError *contentError = nil;
        dispatch_semaphore_t contentReady = dispatch_semaphore_create(0);
        [SCShareableContent getShareableContentExcludingDesktopWindows:NO
                                                  onScreenWindowsOnly:NO
                                                    completionHandler:^(SCShareableContent *shareable, NSError *error) {
            content = shareable;
            contentError = error;
            dispatch_semaphore_signal(contentReady);
        }];
        if (!LumiWait(contentReady, 10.0, @"enumerate shareable displays", &contentError)) {
            if (error_message != NULL) *error_message = LumiCopyError(contentError);
            return NULL;
        }
        if (content == nil) {
            if (error_message != NULL) *error_message = LumiCopyError(contentError);
            return NULL;
        }

        NSString *directoryPath = [NSString stringWithUTF8String:directory];
        NSString *filePrefix = [NSString stringWithUTF8String:prefix];
        NSMutableArray *frames = [NSMutableArray arrayWithCapacity:content.displays.count];
        NSMutableArray<NSString *> *captureErrors = [NSMutableArray array];

        NSMutableArray<NSNumber *> *present = [NSMutableArray arrayWithCapacity:content.displays.count];
        for (SCDisplay *display in content.displays) [present addObject:@(display.displayID)];
        BOOL selectionFallback = NO;
        NSSet<NSNumber *> *selected = [NSSet setWithArray:
            LumiSelectDisplays(present, LumiParseDisplayAllowlist(display_ids_csv), &selectionFallback)];

        for (SCDisplay *display in content.displays) {
            if (![selected containsObject:@(display.displayID)]) continue;
            @autoreleasepool {
                SCContentFilter *filter = [[SCContentFilter alloc] initWithDisplay:display
                                                             excludingApplications:@[]
                                                                  exceptingWindows:@[]];
                SCShareableContentInfo *info = [SCShareableContent infoForFilter:filter];
                CGFloat scale = MAX(1.0, info.pointPixelScale);
                size_t pixelWidth = (size_t)llround((double)display.width * scale);
                size_t pixelHeight = (size_t)llround((double)display.height * scale);
                // A capped width is how `lumi displays` gets its thumbnails: the
                // same capture path, asked for a smaller image, rather than a
                // second capture path or a resize of the full-resolution shot.
                if (max_pixel_width > 0 && pixelWidth > (size_t)max_pixel_width) {
                    double ratio = (double)max_pixel_width / (double)pixelWidth;
                    pixelWidth = (size_t)max_pixel_width;
                    pixelHeight = (size_t)MAX(1LL, llround((double)pixelHeight * ratio));
                }
                SCStreamConfiguration *configuration = [[SCStreamConfiguration alloc] init];
                configuration.width = pixelWidth;
                configuration.height = pixelHeight;
                configuration.showsCursor = YES;
                configuration.shouldBeOpaque = YES;
                configuration.captureResolution = SCCaptureResolutionBest;

                __block CGImageRef capturedImage = NULL;
                __block NSError *captureError = nil;
                dispatch_semaphore_t imageReady = dispatch_semaphore_create(0);
                [SCScreenshotManager captureImageWithFilter:filter
                                              configuration:configuration
                                          completionHandler:^(CGImageRef image, NSError *error) {
                    if (image != NULL) capturedImage = CGImageRetain(image);
                    captureError = error;
                    dispatch_semaphore_signal(imageReady);
                }];
                if (!LumiWait(imageReady, 10.0, @"capture display image", &captureError)) {
                    [captureErrors addObject:captureError.localizedDescription];
                    continue;
                }
                if (capturedImage == NULL) {
                    [captureErrors addObject:captureError.localizedDescription ?: @"capture display image"];
                    continue;
                }

                NSString *filename = [NSString stringWithFormat:@"%@-display-%u.jpg", filePrefix, display.displayID];
                NSString *path = [directoryPath stringByAppendingPathComponent:filename];
                NSError *writeError = nil;
                BOOL wrote = LumiWriteJPEG(capturedImage, path, &writeError);
                size_t width = CGImageGetWidth(capturedImage);
                size_t height = CGImageGetHeight(capturedImage);
                CGImageRelease(capturedImage);
                if (!wrote) {
                    [captureErrors addObject:writeError.localizedDescription ?: @"write display JPEG"];
                    continue;
                }
                [frames addObject:[@{@"path": path,
                                     @"display_id": @(display.displayID),
                                     @"width": @(width),
                                     @"height": @(height)} mutableCopy]];
            }
        }
        if (frames.count == 0 && captureErrors.count > 0) {
            if (error_message != NULL) *error_message = LumiCopyUTF8([captureErrors componentsJoinedByString:@"; "]);
            return NULL;
        }
        if (captureErrors.count > 0) {
            NSString *combined = [captureErrors componentsJoinedByString:@"; "];
            for (NSMutableDictionary *frame in frames) frame[@"capture_error"] = combined;
        }
        // Stamped on every frame the way capture_error is, and kept separate from
        // it: a selection that could not be honoured is degraded capture, not
        // failed capture, and the caller answers the two differently.
        if (selectionFallback) {
            for (NSMutableDictionary *frame in frames) frame[@"selection_fallback"] = @YES;
        }
        NSError *jsonError = nil;
        NSString *json = LumiJSONString(frames, &jsonError);
        if (json == nil) {
            if (error_message != NULL) *error_message = LumiCopyError(jsonError);
            return NULL;
        }
        return LumiCopyUTF8(json);
    }
}

static NSString *LumiAXString(AXUIElementRef element, CFStringRef attribute) {
    CFTypeRef value = NULL;
    if (AXUIElementCopyAttributeValue(element, attribute, &value) != kAXErrorSuccess || value == NULL) {
        return @"";
    }
    NSString *result = @"";
    if (CFGetTypeID(value) == CFStringGetTypeID()) {
        result = [(__bridge NSString *)value copy];
    } else if (CFGetTypeID(value) == CFNumberGetTypeID()) {
        result = [(__bridge NSNumber *)value stringValue];
    }
    CFRelease(value);
    return result ?: @"";
}

static void LumiCollectAXText(AXUIElementRef element, NSMutableOrderedSet<NSString *> *lines,
                              NSUInteger depth, NSUInteger *visited) {
    if (element == NULL || depth > 14 || *visited >= 2500) return;
    (*visited)++;
    NSString *subrole = LumiAXString(element, kAXSubroleAttribute);
    if ([subrole isEqualToString:(__bridge NSString *)kAXSecureTextFieldSubrole]) return;

    for (NSString *attribute in @[(__bridge NSString *)kAXTitleAttribute,
                                  (__bridge NSString *)kAXDescriptionAttribute,
                                  (__bridge NSString *)kAXValueAttribute]) {
        NSString *text = [LumiAXString(element, (__bridge CFStringRef)attribute) stringByTrimmingCharactersInSet:
                          [NSCharacterSet whitespaceAndNewlineCharacterSet]];
        if (text.length > 0 && text.length <= 10000) [lines addObject:text];
    }
    if (lines.description.length >= 120000) return;

    CFTypeRef childrenValue = NULL;
    if (AXUIElementCopyAttributeValue(element, kAXChildrenAttribute, &childrenValue) != kAXErrorSuccess ||
        childrenValue == NULL) return;
    if (CFGetTypeID(childrenValue) == CFArrayGetTypeID()) {
        NSArray *children = (__bridge NSArray *)childrenValue;
        for (id child in children) {
            LumiCollectAXText((__bridge AXUIElementRef)child, lines, depth + 1, visited);
            if (*visited >= 2500) break;
        }
    }
    CFRelease(childrenValue);
}

// LumiDisplayIDForRect maps a window rectangle to the display containing its
// centre. Both Accessibility positions and kCGWindowBounds are expressed in
// top-left-origin global coordinates, so the same lookup serves both callers.
static CGDirectDisplayID LumiDisplayIDForRect(CGRect frame) {
    CGPoint center = CGPointMake(CGRectGetMidX(frame), CGRectGetMidY(frame));
    CGDirectDisplayID displays[32];
    uint32_t count = 0;
    if (CGGetOnlineDisplayList(32, displays, &count) != kCGErrorSuccess) return 0;
    for (uint32_t i = 0; i < count; i++) {
        if (CGRectContainsPoint(CGDisplayBounds(displays[i]), center)) return displays[i];
    }
    return 0;
}

static CGDirectDisplayID LumiAXDisplayID(AXUIElementRef window) {
    CFTypeRef positionValue = NULL;
    CFTypeRef sizeValue = NULL;
    CGPoint position = CGPointZero;
    CGSize size = CGSizeZero;
    if (AXUIElementCopyAttributeValue(window, kAXPositionAttribute, &positionValue) != kAXErrorSuccess ||
        positionValue == NULL || CFGetTypeID(positionValue) != AXValueGetTypeID() ||
        !AXValueGetValue((AXValueRef)positionValue, kAXValueCGPointType, &position)) {
        if (positionValue != NULL) CFRelease(positionValue);
        return 0;
    }
    if (AXUIElementCopyAttributeValue(window, kAXSizeAttribute, &sizeValue) != kAXErrorSuccess ||
        sizeValue == NULL || CFGetTypeID(sizeValue) != AXValueGetTypeID() ||
        !AXValueGetValue((AXValueRef)sizeValue, kAXValueCGSizeType, &size)) {
        CFRelease(positionValue);
        if (sizeValue != NULL) CFRelease(sizeValue);
        return 0;
    }
    CFRelease(positionValue);
    CFRelease(sizeValue);
    return LumiDisplayIDForRect(CGRectMake(position.x, position.y, size.width, size.height));
}

static NSString *const LumiAppSourceAccessibility = @"accessibility";
static NSString *const LumiAppSourceFrontmostValidated = @"accessibility_frontmost";
static NSString *const LumiAppSourceWindowList = @"window_list";
static NSString *const LumiAppSourceWorkspace = @"workspace";
static NSString *const LumiAppSourceRunningApplication = @"running_application";

// LumiSystemWideActivationPID asks the system-wide Accessibility element which
// application holds focus. This is an *activation* source, so unlike the window
// list it is correct for an application with no on-screen window — a Finder with
// every window closed, or an app whose windows are all minimized.
//
// It returns 0 rather than an error because it is genuinely unreliable: it needs
// Accessibility trust, and it fails per-focused-application, returning
// kAXErrorNotImplemented for some apps while succeeding for others in the same
// session. Measured over 45 samples it failed 10 times. Retrying does not help —
// three attempts 30ms apart return byte-identical errors — so the remedy is a
// second source, not a second attempt.
static pid_t LumiSystemWideActivationPID(void) {
    AXUIElementRef systemWide = AXUIElementCreateSystemWide();
    AXUIElementSetMessagingTimeout(systemWide, 1.0);
    CFTypeRef application = NULL;
    AXError axError = AXUIElementCopyAttributeValue(systemWide, kAXFocusedApplicationAttribute, &application);
    pid_t pid = 0;
    if (axError == kAXErrorSuccess && application != NULL) {
        if (AXUIElementGetPid((AXUIElementRef)application, &pid) != kAXErrorSuccess) pid = 0;
        CFRelease(application);
    }
    CFRelease(systemWide);
    return pid;
}

// LumiFrontmostValidatedPID validates window-list candidates against activation
// instead of trusting window order. It walks the list front-to-back and asks
// each distinct owner, over per-application Accessibility, whether it is itself
// frontmost — returning the first that says yes.
//
// This exists because window order and activation disagree exactly at app-switch
// boundaries, which is where the residual misattribution was concentrated: the
// top layer-0 window is briefly the *previous* app (or the *next* one) while
// something else is actually active, and events captured in that gap were
// stamped with whichever window happened to be on top. Where the system-wide
// read failed in 10 of 45 samples, this answered in 45 of 45 — including the
// switch-boundary sample where the window list said Messages and the frontmost
// application was still Claude.
//
// It is bounded and short-circuits: the true frontmost application is normally
// the first or second candidate, so this is one or two AX round-trips per tick.
// kAXFrontmostAttribute is a per-application read, which the misattributed
// events prove keeps working even when the system-wide element does not.
//
// LumiFrontmostCandidates builds the ordered list to ask. It is pure, so which
// processes are eligible — the question behind this whole subsystem — is
// testable without a live session.
//
// On-screen window owners come first, front-to-back, because the frontmost
// application almost always owns the front window and that ends the walk in one
// round-trip. regularPIDs follows, and exists because an application with no
// on-screen window at all — every window minimized or closed — owns no entry in
// the window list and could otherwise never be discovered, leaving exactly the
// activation-versus-visibility misattribution this subsystem exists to prevent.
//
// regularPIDs must be dock-visible applications only (NSApplicationActivation-
// PolicyRegular). Widening it to every running application is the obvious
// generalisation and is wrong: background agents answer kAXFrontmost
// affirmatively, and an unfiltered walk was measured attributing frames to
// Notification Center. Filtered to regular applications, no spurious claimant
// appeared in any sample, and simulating a windowless active application
// recovered it in 19 of 20.
static NSArray<NSNumber *> *LumiFrontmostCandidates(NSArray *windows, NSArray<NSNumber *> *regularPIDs,
                                                    pid_t selfPID) {
    NSMutableArray<NSNumber *> *candidates = [NSMutableArray array];
    NSMutableSet<NSNumber *> *seen = [NSMutableSet set];
    for (NSDictionary *window in windows) {
        if (![window isKindOfClass:NSDictionary.class]) continue;
        NSNumber *owner = window[(__bridge NSString *)kCGWindowOwnerPID];
        if (owner == nil || [seen containsObject:owner]) continue;
        pid_t pid = (pid_t)owner.intValue;
        if (pid == 0 || pid == selfPID) continue;
        [seen addObject:owner];
        [candidates addObject:owner];
        if (candidates.count >= 8) break;
    }
    for (NSNumber *candidate in regularPIDs) {
        if (candidate == nil || [seen containsObject:candidate]) continue;
        pid_t pid = (pid_t)candidate.intValue;
        if (pid == 0 || pid == selfPID) continue;
        [seen addObject:candidate];
        [candidates addObject:candidate];
        if (candidates.count >= 48) break;
    }
    return candidates;
}

// LumiRegularApplicationPIDs is the dock-visible half of LumiFrontmostCandidates.
// The list itself comes from NSWorkspace, which is why it is read here and not
// in the pure function: only membership is used, never activation state, since
// runningApplications/isActive freezes in a process that runs no run loop.
static NSArray<NSNumber *> *LumiRegularApplicationPIDs(void) {
    NSMutableArray<NSNumber *> *pids = [NSMutableArray array];
    for (NSRunningApplication *application in NSWorkspace.sharedWorkspace.runningApplications) {
        if (application.activationPolicy != NSApplicationActivationPolicyRegular) continue;
        [pids addObject:@(application.processIdentifier)];
    }
    return pids;
}

static pid_t LumiFrontmostValidatedPID(NSArray *windows, pid_t selfPID) {
    NSArray<NSNumber *> *candidates =
        LumiFrontmostCandidates(windows, LumiRegularApplicationPIDs(), selfPID);
    for (NSNumber *candidate in candidates) {
        AXUIElementRef application = AXUIElementCreateApplication((pid_t)candidate.intValue);
        AXUIElementSetMessagingTimeout(application, 0.5);
        CFTypeRef frontmost = NULL;
        AXError axError = AXUIElementCopyAttributeValue(application, kAXFrontmostAttribute, &frontmost);
        BOOL isFrontmost = NO;
        if (axError == kAXErrorSuccess && frontmost != NULL) {
            isFrontmost = CFBooleanGetValue((CFBooleanRef)frontmost);
            CFRelease(frontmost);
        }
        CFRelease(application);
        if (isFrontmost) return (pid_t)candidate.intValue;
    }
    return 0;
}

// LumiActivationPID resolves activation from the system-wide element, falling
// back to validating window-list candidates. viaValidation reports which
// answered, so the provenance recorded on the event stays honest.
static pid_t LumiActivationPID(NSArray *windows, pid_t selfPID, BOOL *viaValidation) {
    if (viaValidation != NULL) *viaValidation = NO;
    pid_t pid = LumiSystemWideActivationPID();
    if (pid != 0) return pid;
    pid = LumiFrontmostValidatedPID(windows, selfPID);
    if (pid != 0 && viaValidation != NULL) *viaValidation = YES;
    return pid;
}

// LumiResolveFrontmost decides which process a captured frame is attributed to.
// It is pure — it makes no CoreGraphics and no NSWorkspace call — so the branch
// order it encodes is testable off a live session, the way LumiHIDAccessName is.
//
// The order is Accessibility, then the window list, then NSWorkspace — and
// NSWorkspace coming last is the opposite of the obvious ordering. Its
// frontmostApplication is backed by activation state maintained through
// notification delivery on a run loop. The recorder is a detached daemon
// (record_daemon.go sets Setsid) that never runs one, so that value freezes at
// whichever app was frontmost when the process started — the terminal that
// launched it. Every event then names that terminal, while the window title,
// read live against its stale pid, keeps advancing; a live-looking title is
// therefore no evidence the app is right. Measured over a minute of real focus
// changes, frontmostApplication never moved off its start value, and
// runningApplications/isActive froze identically — so neither can lead.
//
// activePID (from LumiActivationPID) leads when it is available because it is
// the only source that answers *activation* rather than *visibility*: an app
// whose windows are all closed or minimized is still what the user is working
// in, and CLAUDE.md defines attribution as "what was the user working in", not
// "what is shown in this image". The window list cannot see such an app and
// would name whatever is visually behind it. The window list still leads over
// NSWorkspace: it is copied fresh per call, cannot go stale, and needs only
// Screen Recording, which is definitionally granted whenever there is a frame
// to attribute.
static NSDictionary *LumiResolveFrontmost(NSArray *windows, pid_t activePID,
                                          pid_t workspacePID, NSString *workspaceName,
                                          pid_t selfPID) {
    NSString *fallbackName = workspaceName ?: @"";
    if (activePID != 0 && activePID != selfPID) {
        // The name is completed by the caller from the pid when no source here
        // knows it; a pid alone is still correct attribution the caller can fix.
        NSString *name = @"";
        for (NSDictionary *window in windows) {
            if (![window isKindOfClass:NSDictionary.class]) continue;
            NSNumber *pid = window[(__bridge NSString *)kCGWindowOwnerPID];
            if (pid == nil || (pid_t)pid.intValue != activePID) continue;
            NSString *owner = window[(__bridge NSString *)kCGWindowOwnerName];
            if (owner.length > 0) { name = owner; break; }
        }
        if (name.length == 0 && activePID == workspacePID) name = fallbackName;
        return @{@"pid": @(activePID), @"app": name, @"app_source": LumiAppSourceAccessibility};
    }
    for (NSDictionary *window in windows) {
        if (![window isKindOfClass:NSDictionary.class]) continue;
        NSNumber *pid = window[(__bridge NSString *)kCGWindowOwnerPID];
        NSNumber *layer = window[(__bridge NSString *)kCGWindowLayer];
        // Layer 0 excludes the menu bar, status items, notification banners and
        // window shadows, so attribution lands on the app whose pixels the
        // full-display OCR actually captured.
        if (pid == nil || layer.intValue != 0) continue;
        pid_t owner = (pid_t)pid.intValue;
        if (owner == 0 || owner == selfPID) continue;
        NSString *name = window[(__bridge NSString *)kCGWindowOwnerName];
        if (name.length == 0) {
            // A name is borrowable from NSWorkspace only when both sources mean
            // the same process. Borrowing it across differing pids would name
            // one app while reading another's title — the exact bug this
            // function exists to prevent — so leave it to the caller instead.
            name = (owner == workspacePID) ? fallbackName : @"";
        }
        return @{@"pid": @(owner), @"app": name, @"app_source": LumiAppSourceWindowList};
    }
    return @{@"pid": @(workspacePID), @"app": fallbackName, @"app_source": LumiAppSourceWorkspace};
}

// lumi_resolve_frontmost_json exposes the branch order above so every case is
// testable on a machine that can only ever sit in one of them. Asserting the
// live resolution instead would pass vacuously wherever NSWorkspace happens to
// be correct — which is every foreground process, and so every test run.
char *lumi_resolve_frontmost_json(const char *windows_json, int active_pid, int workspace_pid,
                                  const char *workspace_app, int self_pid,
                                  char **error_message) {
    @autoreleasepool {
        NSArray *windows = nil;
        if (windows_json != NULL) {
            NSData *data = [@(windows_json) dataUsingEncoding:NSUTF8StringEncoding];
            NSError *parseError = nil;
            id decoded = [NSJSONSerialization JSONObjectWithData:data
                                                         options:NSJSONReadingAllowFragments
                                                           error:&parseError];
            if (decoded == nil) {
                if (error_message != NULL) *error_message = LumiCopyError(parseError);
                return NULL;
            }
            // A JSON null decodes to NSNull, not an array: that is the "the
            // window list could not be copied" case, and it must fall back.
            if ([decoded isKindOfClass:NSArray.class]) windows = decoded;
        }
        NSDictionary *resolved = LumiResolveFrontmost(
            windows, (pid_t)active_pid, (pid_t)workspace_pid,
            workspace_app == NULL ? @"" : @(workspace_app), (pid_t)self_pid);
        NSError *jsonError = nil;
        NSString *json = LumiJSONString(resolved, &jsonError);
        if (json == nil) {
            if (error_message != NULL) *error_message = LumiCopyError(jsonError);
            return NULL;
        }
        return LumiCopyUTF8(json);
    }
}

// lumi_frontmost_candidates_json exposes LumiFrontmostCandidates so the
// eligibility rule is testable on a machine that cannot be posed into the case
// that matters. The gap it guards — an active application owning no window —
// cannot be asserted by supplying its pid to the resolver, because that is the
// step under test: what must be proved is that such a process is *reachable*.
char *lumi_frontmost_candidates_json(const char *windows_json, const char *regular_pids_json,
                                     int self_pid, char **error_message) {
    @autoreleasepool {
        NSArray *windows = nil, *regular = nil;
        NSError *parseError = nil;
        if (windows_json != NULL) {
            id decoded = [NSJSONSerialization JSONObjectWithData:[@(windows_json) dataUsingEncoding:NSUTF8StringEncoding]
                                                         options:NSJSONReadingAllowFragments error:&parseError];
            if (decoded == nil) {
                if (error_message != NULL) *error_message = LumiCopyError(parseError);
                return NULL;
            }
            if ([decoded isKindOfClass:NSArray.class]) windows = decoded;
        }
        if (regular_pids_json != NULL) {
            id decoded = [NSJSONSerialization JSONObjectWithData:[@(regular_pids_json) dataUsingEncoding:NSUTF8StringEncoding]
                                                         options:NSJSONReadingAllowFragments error:&parseError];
            if (decoded == nil) {
                if (error_message != NULL) *error_message = LumiCopyError(parseError);
                return NULL;
            }
            if ([decoded isKindOfClass:NSArray.class]) regular = decoded;
        }
        NSArray *candidates = LumiFrontmostCandidates(windows, regular, (pid_t)self_pid);
        NSError *jsonError = nil;
        NSString *json = LumiJSONString(candidates, &jsonError);
        if (json == nil) {
            if (error_message != NULL) *error_message = LumiCopyError(jsonError);
            return NULL;
        }
        return LumiCopyUTF8(json);
    }
}

// LumiResolveFrontmostLive completes LumiResolveFrontmost against the frameworks
// it deliberately does not touch. The pure function can name a pid it cannot
// name an app for; only here may that be repaired.
static NSDictionary *LumiResolveFrontmostLive(NSArray *windows, pid_t activePID,
                                              BOOL activeViaValidation,
                                              NSRunningApplication *frontmost) {
    pid_t workspacePID = frontmost == nil ? 0 : frontmost.processIdentifier;
    NSString *workspaceName = frontmost.localizedName ?: @"";
    NSMutableDictionary *resolved =
        [LumiResolveFrontmost(windows, activePID, workspacePID, workspaceName, getpid()) mutableCopy];
    // The pure function cannot tell which Accessibility read answered, and the
    // two have very different reliability, so the distinction is recorded here.
    if (activeViaValidation && [resolved[@"app_source"] isEqualToString:LumiAppSourceAccessibility]) {
        resolved[@"app_source"] = LumiAppSourceFrontmostValidated;
    }
    if ([resolved[@"app"] length] > 0) return resolved;

    pid_t pid = (pid_t)[resolved[@"pid"] intValue];
    NSString *name = pid == 0 ? nil :
        [NSRunningApplication runningApplicationWithProcessIdentifier:pid].localizedName;
    if (name.length > 0) {
        resolved[@"app"] = name;
        resolved[@"app_source"] = LumiAppSourceRunningApplication;
    } else if (workspaceName.length > 0) {
        // Fall back to the workspace pair *wholesale*, pid included. Keeping the
        // window list's pid while borrowing NSWorkspace's name would attribute
        // one app's title to another app's name; a stale-but-consistent pair is
        // the lesser failure, and app_source says which one this is.
        resolved[@"pid"] = @(workspacePID);
        resolved[@"app"] = workspaceName;
        resolved[@"app_source"] = LumiAppSourceWorkspace;
    }
    return resolved;
}

// LumiWindowListTitleIn is the attribution fallback for when the Accessibility
// tree cannot be read: an untrusted process, a wedged AX message, or a
// Chromium/Electron app that has not built its tree. kCGWindowName is populated
// only for clients holding Screen Recording, which is definitionally granted
// whenever there is a captured frame to attribute.
//
// The window list is ordered front-to-back, so the first layer-0 window owned by
// the frontmost process is its focused window. Layer 0 excludes the menu bar,
// status items and window shadows. It takes the array rather than copying one so
// that the title and the app name resolved above come from the same snapshot of
// the window server, and so the copy is made once per tick rather than twice.
static NSString *LumiWindowListTitleIn(NSArray *windows, pid_t owner, CGDirectDisplayID *displayID) {
    NSString *title = nil;
    for (NSDictionary *window in windows) {
        NSNumber *pid = window[(__bridge NSString *)kCGWindowOwnerPID];
        NSNumber *layer = window[(__bridge NSString *)kCGWindowLayer];
        if (pid.intValue != (int)owner || layer.intValue != 0) continue;
        // kCGWindowName is absent rather than empty for some applications; a
        // window with no readable title still fixes the display, so keep going
        // only until the first matching window is found.
        title = window[(__bridge NSString *)kCGWindowName];
        if (displayID != NULL) {
            CGRect bounds = CGRectZero;
            CFDictionaryRef boundsDict =
                (__bridge CFDictionaryRef)window[(__bridge NSString *)kCGWindowBounds];
            if (boundsDict != NULL && CGRectMakeWithDictionaryRepresentation(boundsDict, &bounds)) {
                *displayID = LumiDisplayIDForRect(bounds);
            }
        }
        break;
    }
    return title;
}

// LumiAudioPlayingMarker is the suffix Chromium appends to a window title while
// one of its tabs is playing sound. Measured on a live index: 116 of 117 Comet
// events captured during playback carried it, e.g.
// "(45) Why Intelligence Always Escapes … - YouTube - Audio playing - Comet".
//
// The test is containment, not suffix: the marker sits *before* the browser name,
// so a suffix comparison never matches.
static NSString *const LumiAudioPlayingMarker = @" - Audio playing";

// LumiAudioMarkerWindowsIn reports every on-screen window whose own title says it
// is playing audio.
//
// It exists as a fallback for the case CoreAudio cannot answer, and it scans
// *all* windows rather than just the frontmost one — which is the entire point.
// The frontmost window is the one case where the answer is already known to be
// unreliable: attributing sound to whatever is focused is the defect this whole
// change removes. What matters is the browser playing a video in the background
// while the user works somewhere else.
//
// Only windows carrying the marker leave this function, and the marker is
// stripped from what they carry. Every other title read here is discarded
// in-place: this is not a window-title harvester, it is a test for a
// self-declared emitter.
//
// kCGWindowName is populated only for clients holding Screen Recording, which
// the recorder holds whenever it has audio to attribute.
static NSArray<NSDictionary *> *LumiAudioMarkerWindowsIn(NSArray *windows) {
    NSMutableArray<NSDictionary *> *found = [NSMutableArray array];
    NSMutableSet<NSNumber *> *seen = [NSMutableSet set];
    pid_t selfPID = getpid();
    for (NSDictionary *window in windows) {
        if (![window isKindOfClass:NSDictionary.class]) continue;
        NSNumber *layer = window[(__bridge NSString *)kCGWindowLayer];
        if (layer.intValue != 0) continue;
        NSString *title = window[(__bridge NSString *)kCGWindowName];
        if (![title isKindOfClass:NSString.class] || title.length == 0) continue;
        if ([title rangeOfString:LumiAudioPlayingMarker].location == NSNotFound) continue;
        NSNumber *owner = window[(__bridge NSString *)kCGWindowOwnerPID];
        if (owner == nil) continue;
        pid_t pid = (pid_t)owner.intValue;
        // Lumi's own output is excluded from the capture session
        // (excludesCurrentProcessAudio), so naming it would claim provenance for
        // sound the recording cannot contain — the same reason AudioProcesses
        // filters it.
        if (pid == 0 || pid == selfPID) continue;
        // One entry per application. A browser with three noisy tabs is one
        // emitter, and counting it three times would weight it above a genuinely
        // separate application.
        if ([seen containsObject:owner]) continue;
        [seen addObject:owner];

        NSString *stripped = [title stringByReplacingOccurrencesOfString:LumiAudioPlayingMarker
                                                              withString:@""];
        NSMutableDictionary *entry = [@{@"pid": @(pid), @"window": stripped} mutableCopy];
        NSString *ownerName = window[(__bridge NSString *)kCGWindowOwnerName];
        NSRunningApplication *application =
            [NSRunningApplication runningApplicationWithProcessIdentifier:pid];
        NSString *name = ownerName.length > 0 ? ownerName : application.localizedName;
        if (name.length > 0) entry[@"name"] = name;
        if (application.bundleIdentifier.length > 0) {
            entry[@"bundle_id"] = application.bundleIdentifier;
        }
        [found addObject:entry];
    }
    return found;
}

// lumi_audio_marker_windows_json exposes the scan above. It needs no
// Accessibility grant: the window list is a Screen Recording read, which is
// already held wherever there is captured audio to attribute.
char *lumi_audio_marker_windows_json(char **error_message) {
    @autoreleasepool {
        CFArrayRef windowList = CGWindowListCopyWindowInfo(
            kCGWindowListOptionOnScreenOnly | kCGWindowListExcludeDesktopElements, kCGNullWindowID);
        // A window list that could not be copied is a *failure to sample*, not a
        // finding that nothing is playing. Returning an empty array here would
        // record "no application was emitting" for a scan that never ran, and the
        // recorder could not tell the two apart afterwards — the same distinction
        // an absent source list keeps against its _error sibling.
        if (windowList == NULL) {
            if (error_message != NULL) {
                *error_message = LumiCopyUTF8(@"copy the on-screen window list");
            }
            return NULL;
        }
        NSArray *windows = (__bridge NSArray *)windowList;
        NSArray<NSDictionary *> *found = LumiAudioMarkerWindowsIn(windows);
        CFRelease(windowList);
        NSError *jsonError = nil;
        NSString *json = LumiJSONString(found, &jsonError);
        if (json == nil) {
            if (error_message != NULL) *error_message = LumiCopyError(jsonError);
            return NULL;
        }
        return LumiCopyUTF8(json);
    }
}

// lumi_audio_marker_windows_in_json is the pure resolver behind the live entry
// point, exposed for the same reason the other *_in_json resolvers are: asserting
// the live scan passes vacuously in any process that happens to have nothing
// playing, so it would only ever fail where nothing is asserting.
char *lumi_audio_marker_windows_in_json(const char *windows_json, char **error_message) {
    @autoreleasepool {
        NSArray *windows = nil;
        if (windows_json != NULL) {
            NSError *parseError = nil;
            id decoded = [NSJSONSerialization
                JSONObjectWithData:[@(windows_json) dataUsingEncoding:NSUTF8StringEncoding]
                           options:NSJSONReadingAllowFragments
                             error:&parseError];
            if (decoded == nil) {
                if (error_message != NULL) *error_message = LumiCopyError(parseError);
                return NULL;
            }
            // A JSON null decodes to NSNull, not an array: that is the "the window
            // list could not be copied" case, and it yields no markers rather than
            // an error.
            if ([decoded isKindOfClass:NSArray.class]) windows = decoded;
        }
        NSError *jsonError = nil;
        NSString *json = LumiJSONString(LumiAudioMarkerWindowsIn(windows), &jsonError);
        if (json == nil) {
            if (error_message != NULL) *error_message = LumiCopyError(jsonError);
            return NULL;
        }
        return LumiCopyUTF8(json);
    }
}

// lumi_accessibility_snapshot_json is deliberately total: every field that can be
// obtained without an Accessibility grant is gathered before the first AX call,
// and an AX failure degrades the snapshot rather than discarding it. Returning
// NULL here costs the caller the frontmost application name, which NSWorkspace
// hands over for free — that loss is what left months of events unattributed.
//
// NULL is reserved for a genuine total failure: no source could name the
// frontmost process at all, or a payload that will not serialize.
char *lumi_accessibility_snapshot_json(char **error_message) {
    @autoreleasepool {
        BOOL inputActive = CGEventSourceSecondsSinceLastEventType(kCGEventSourceStateCombinedSessionState,
                                                                  kCGAnyInputEventType) < 2.0;
        // Trust is sampled from the running process on every tick. A one-shot
        // check at startup cannot distinguish "trust was revoked mid-run" from
        // "AX messaging is wedged", and those have different remedies.
        BOOL trusted = AXIsProcessTrusted();

        // The window list is copied once, before the first AX call, and is the
        // primary source of the frontmost pid — see LumiResolveFrontmost for why
        // NSWorkspace cannot be trusted for it in this process. The same array
        // then serves the title fallback below.
        CFArrayRef windowList = CGWindowListCopyWindowInfo(
            kCGWindowListOptionOnScreenOnly | kCGWindowListExcludeDesktopElements, kCGNullWindowID);
        NSArray *windows = windowList == NULL ? nil : (__bridge NSArray *)windowList;
        BOOL activeViaValidation = NO;
        pid_t activePID = LumiActivationPID(windows, getpid(), &activeViaValidation);
        NSDictionary *resolved = LumiResolveFrontmostLive(
            windows, activePID, activeViaValidation, NSWorkspace.sharedWorkspace.frontmostApplication);
        pid_t frontmostPID = (pid_t)[resolved[@"pid"] intValue];
        NSString *frontmostApp = resolved[@"app"];
        // Both sources failed: there is nothing to attribute and nothing to
        // read a title against. Anything less than this stays a snapshot,
        // because a snapshot naming an app is worth more than an error.
        if (frontmostApp.length == 0 && frontmostPID == 0) {
            if (windowList != NULL) CFRelease(windowList);
            if (error_message != NULL) *error_message = LumiCopyUTF8(@"no frontmost application");
            return NULL;
        }
        NSMutableDictionary *snapshot = [@{@"app": frontmostApp,
                                           @"window": @"",
                                           @"text": @"",
                                           @"display_id": @(0),
                                           @"input_active": @(inputActive),
                                           @"trusted": @(trusted),
                                           @"app_source": resolved[@"app_source"],
                                           @"title_source": @"none"} mutableCopy];

        AXUIElementRef application = AXUIElementCreateApplication(frontmostPID);
        AXUIElementSetMessagingTimeout(application, 1.0);
        CFTypeRef windowValue = NULL;
        AXError windowError = AXUIElementCopyAttributeValue(application, kAXFocusedWindowAttribute, &windowValue);
        if (windowError == kAXErrorSuccess && windowValue != NULL) {
            AXUIElementRef window = (AXUIElementRef)windowValue;
            NSString *title = LumiAXString(window, kAXTitleAttribute);
            NSMutableOrderedSet<NSString *> *lines = [NSMutableOrderedSet orderedSet];
            NSUInteger visited = 0;
            LumiCollectAXText(window, lines, 0, &visited);
            snapshot[@"window"] = title ?: @"";
            snapshot[@"text"] = [lines.array componentsJoinedByString:@"\n"] ?: @"";
            snapshot[@"display_id"] = @(LumiAXDisplayID(window));
            snapshot[@"title_source"] = @"accessibility";
            CFRelease(windowValue);
        } else {
            snapshot[@"error"] = [NSString stringWithFormat:
                                  @"read focused Accessibility window (AX error %d)", windowError];
            CGDirectDisplayID displayID = 0;
            NSString *title = LumiWindowListTitleIn(windows, frontmostPID, &displayID);
            if (title != nil) {
                snapshot[@"window"] = title;
                snapshot[@"display_id"] = @(displayID);
                snapshot[@"title_source"] = @"window_list";
            }
        }
        CFRelease(application);
        if (windowList != NULL) CFRelease(windowList);

        NSError *jsonError = nil;
        NSString *json = LumiJSONString(snapshot, &jsonError);
        if (json == nil) {
            if (error_message != NULL) *error_message = LumiCopyError(jsonError);
            return NULL;
        }
        return LumiCopyUTF8(json);
    }
}

// lumi_frontmost_diagnostic_json reports the two frontmost sources side by side.
// It runs the same LumiResolveFrontmost the snapshot does, so the diagnostic and
// the behaviour it describes cannot drift apart.
//
// The disagreement it exists to surface only appears in a process launched by
// one app while another is frontmost — which is the recorder daemon's situation
// and not a foreground test's, so this is reported rather than asserted.
char *lumi_frontmost_diagnostic_json(char **error_message) {
    @autoreleasepool {
        CFArrayRef windowList = CGWindowListCopyWindowInfo(
            kCGWindowListOptionOnScreenOnly | kCGWindowListExcludeDesktopElements, kCGNullWindowID);
        NSArray *windows = windowList == NULL ? nil : (__bridge NSArray *)windowList;
        NSRunningApplication *frontmost = NSWorkspace.sharedWorkspace.frontmostApplication;
        pid_t workspacePID = frontmost == nil ? 0 : frontmost.processIdentifier;
        NSString *workspaceName = frontmost.localizedName ?: @"";

        // Resolving with no activation pid and no workspace pair isolates what
        // the window list alone knows; a "workspace" verdict here means the list
        // named nothing.
        NSDictionary *listOnly = LumiResolveFrontmost(windows, 0, 0, @"", getpid());
        BOOL listNamed = [listOnly[@"app_source"] isEqualToString:LumiAppSourceWindowList];
        BOOL activeViaValidation = NO;
        pid_t activePID = LumiActivationPID(windows, getpid(), &activeViaValidation);
        NSString *activeName = activePID == 0 ? @"" :
            ([NSRunningApplication runningApplicationWithProcessIdentifier:activePID].localizedName ?: @"");
        NSDictionary *resolved =
            LumiResolveFrontmostLive(windows, activePID, activeViaValidation, frontmost);
        if (windowList != NULL) CFRelease(windowList);

        // Boxed through a BOOL local deliberately: @(a && b) boxes a C int and
        // serializes as 0/1, which decodes as a number rather than a bool.
        BOOL agree = listNamed && [listOnly[@"pid"] intValue] == (int)workspacePID;
        NSDictionary *payload = @{
            @"accessibility": @{@"pid": @(activePID), @"app": activeName},
            @"workspace": @{@"pid": @(workspacePID), @"app": workspaceName},
            @"window_list": @{@"pid": listNamed ? listOnly[@"pid"] : @(0),
                              @"app": listNamed ? listOnly[@"app"] : @""},
            @"resolved": resolved,
            @"agree": @(agree),
        };
        NSError *jsonError = nil;
        NSString *json = LumiJSONString(payload, &jsonError);
        if (json == nil) {
            if (error_message != NULL) *error_message = LumiCopyError(jsonError);
            return NULL;
        }
        return LumiCopyUTF8(json);
    }
}

char *lumi_vision_recognize(const char *image_path, char **error_message) {
    @autoreleasepool {
        NSURL *url = [NSURL fileURLWithPath:[NSString stringWithUTF8String:image_path]];
        CGImageSourceRef source = CGImageSourceCreateWithURL((__bridge CFURLRef)url, NULL);
        if (source == NULL) {
            if (error_message != NULL) *error_message = LumiCopyUTF8(@"open image for Apple Vision");
            return NULL;
        }
        CGImageRef image = CGImageSourceCreateImageAtIndex(source, 0, NULL);
        CFRelease(source);
        if (image == NULL) {
            if (error_message != NULL) *error_message = LumiCopyUTF8(@"decode image for Apple Vision");
            return NULL;
        }
        VNRecognizeTextRequest *request = [[VNRecognizeTextRequest alloc] init];
        request.recognitionLevel = VNRequestTextRecognitionLevelAccurate;
        request.usesLanguageCorrection = YES;
        VNImageRequestHandler *handler = [[VNImageRequestHandler alloc] initWithCGImage:image options:@{}];
        NSError *visionError = nil;
        BOOL performed = [handler performRequests:@[request] error:&visionError];
        CGImageRelease(image);
        if (!performed) {
            if (error_message != NULL) *error_message = LumiCopyError(visionError);
            return NULL;
        }
        NSMutableArray<NSString *> *lines = [NSMutableArray array];
        for (VNRecognizedTextObservation *observation in request.results) {
            VNRecognizedText *candidate = [observation topCandidates:1].firstObject;
            if (candidate.string.length > 0) [lines addObject:candidate.string];
        }
        return LumiCopyUTF8([lines componentsJoinedByString:@"\n"]);
    }
}

static NSString *LumiAuthorizationName(AVAuthorizationStatus status) {
    switch (status) {
        case AVAuthorizationStatusAuthorized: return @"granted";
        case AVAuthorizationStatusDenied: return @"denied";
        case AVAuthorizationStatusRestricted: return @"restricted";
        case AVAuthorizationStatusNotDetermined: return @"not_determined";
    }
    return @"unknown";
}

static NSString *LumiSpeechAuthorizationName(SFSpeechRecognizerAuthorizationStatus status) {
    switch (status) {
        case SFSpeechRecognizerAuthorizationStatusAuthorized: return @"granted";
        case SFSpeechRecognizerAuthorizationStatusDenied: return @"denied";
        case SFSpeechRecognizerAuthorizationStatusRestricted: return @"restricted";
        case SFSpeechRecognizerAuthorizationStatusNotDetermined: return @"not_determined";
    }
    return @"unknown";
}

// LumiHIDAccessName maps IOHIDCheckAccess's tri-state to a status string.
// CGPreflightListenEventAccess wraps the same query in a BOOL and throws the
// distinction away, but "denied" and "not_determined" need opposite remedies:
// a denied subject is never re-prompted and can only be fixed in System
// Settings, while a not-determined one is exactly what `--request` can still
// resolve. Reporting one string for both leaves no way to tell them apart.
static NSString *LumiHIDAccessName(IOHIDAccessType access) {
    switch (access) {
        case kIOHIDAccessTypeGranted: return @"granted";
        case kIOHIDAccessTypeDenied: return @"denied";
        case kIOHIDAccessTypeUnknown: return @"not_determined";
    }
    return @"unknown";
}

// lumi_hid_access_name exposes the mapping above so it can be tested for every
// state on a machine that sits in only one of them.
char *lumi_hid_access_name(int access) {
    @autoreleasepool {
        return LumiCopyUTF8(LumiHIDAccessName((IOHIDAccessType)access));
    }
}

char *lumi_permissions_json(char **error_message) {
    @autoreleasepool {
        // Screen Recording and Accessibility stay conflated on purpose:
        // CGPreflightScreenCaptureAccess and AXIsProcessTrusted return a bare
        // BOOL, and the only ways to split them either need Full Disk Access
        // (reading TCC.db) or raise a prompt as a side effect of asking
        // (SCShareableContent) — unacceptable in a read-only status call.
        NSDictionary *permissions = @{
            @"screen_recording": CGPreflightScreenCaptureAccess() ? @"granted" : @"denied_or_not_determined",
            @"accessibility": AXIsProcessTrusted() ? @"granted" : @"denied_or_not_determined",
            @"input_monitoring": LumiHIDAccessName(IOHIDCheckAccess(kIOHIDRequestTypeListenEvent)),
            @"microphone": LumiAuthorizationName([AVCaptureDevice authorizationStatusForMediaType:AVMediaTypeAudio]),
            @"speech_recognition": LumiSpeechAuthorizationName([SFSpeechRecognizer authorizationStatus]),
        };
        NSError *jsonError = nil;
        NSString *json = LumiJSONString(permissions, &jsonError);
        if (json == nil) {
            if (error_message != NULL) *error_message = LumiCopyError(jsonError);
            return NULL;
        }
        return LumiCopyUTF8(json);
    }
}

char *lumi_request_permissions_json(bool input_monitoring, char **error_message) {
    @autoreleasepool {
        if (!CGPreflightScreenCaptureAccess()) CGRequestScreenCaptureAccess();
        NSDictionary *accessibilityOptions = @{
            (__bridge NSString *)kAXTrustedCheckOptionPrompt: @YES,
        };
        AXIsProcessTrustedWithOptions((__bridge CFDictionaryRef)accessibilityOptions);
        if (input_monitoring && !CGPreflightListenEventAccess()) CGRequestListenEventAccess();

        if ([AVCaptureDevice authorizationStatusForMediaType:AVMediaTypeAudio] ==
            AVAuthorizationStatusNotDetermined) {
            dispatch_semaphore_t microphoneReady = dispatch_semaphore_create(0);
            [AVCaptureDevice requestAccessForMediaType:AVMediaTypeAudio completionHandler:^(BOOL granted) {
                dispatch_semaphore_signal(microphoneReady);
            }];
            dispatch_semaphore_wait(microphoneReady, DISPATCH_TIME_FOREVER);
        }
        if ([SFSpeechRecognizer authorizationStatus] == SFSpeechRecognizerAuthorizationStatusNotDetermined) {
            dispatch_semaphore_t speechReady = dispatch_semaphore_create(0);
            [SFSpeechRecognizer requestAuthorization:^(SFSpeechRecognizerAuthorizationStatus status) {
                dispatch_semaphore_signal(speechReady);
            }];
            dispatch_semaphore_wait(speechReady, DISPATCH_TIME_FOREVER);
        }
        return lumi_permissions_json(error_message);
    }
}

// LumiAudioProcessFlag reads one boolean process property, treating an
// unreadable property as false. A process that will not answer is not evidence
// that it is playing, and the caller has no better remedy than skipping it.
static BOOL LumiAudioProcessFlag(AudioObjectID process, AudioObjectPropertySelector selector) {
    AudioObjectPropertyAddress address = {
        .mSelector = selector,
        .mScope = kAudioObjectPropertyScopeGlobal,
        .mElement = kAudioObjectPropertyElementMain,
    };
    UInt32 value = 0;
    UInt32 size = sizeof(value);
    if (AudioObjectGetPropertyData(process, &address, 0, NULL, &size, &value) != noErr) {
        return NO;
    }
    return value != 0;
}

static pid_t LumiAudioProcessPID(AudioObjectID process) {
    AudioObjectPropertyAddress address = {
        .mSelector = kAudioProcessPropertyPID,
        .mScope = kAudioObjectPropertyScopeGlobal,
        .mElement = kAudioObjectPropertyElementMain,
    };
    pid_t pid = 0;
    UInt32 size = sizeof(pid);
    if (AudioObjectGetPropertyData(process, &address, 0, NULL, &size, &pid) != noErr) {
        return 0;
    }
    return pid;
}

static NSString *LumiAudioProcessBundleID(AudioObjectID process) {
    AudioObjectPropertyAddress address = {
        .mSelector = kAudioProcessPropertyBundleID,
        .mScope = kAudioObjectPropertyScopeGlobal,
        .mElement = kAudioObjectPropertyElementMain,
    };
    CFStringRef bundleID = NULL;
    UInt32 size = sizeof(bundleID);
    if (AudioObjectGetPropertyData(process, &address, 0, NULL, &size, &bundleID) != noErr ||
        bundleID == NULL) {
        return nil;
    }
    NSString *value = (__bridge_transfer NSString *)bundleID;
    return value.length == 0 ? nil : value;
}

// lumi_audio_processes_json lists the processes holding an active audio output
// stream. It is what lets a system-track recording name the application that
// produced it, which no other source can answer: the WAV holds the mixed output
// graph and carries no provenance of its own.
//
// kAudioProcessPropertyIsRunningOutput reports *stream occupancy, not audible
// sound* — a paused player that still holds its stream open answers yes
// (measured with QuickTime; the same app with its document closed answers no).
// That is the strongest claim available here, and the caller's field names say
// so. The per-process property set is closed — PID, BundleID, Devices,
// IsRunning, IsRunningInput, IsRunningOutput — and none of it carries level, so
// establishing real emission would need AudioHardwareCreateProcessTap.
//
// This is a *read* of CoreAudio's process objects, never a tap. Nothing here
// captures audio, which is the distinction that matters for permissions —
// creating a process tap requires a TCC grant, enumerating the objects does not.
//
// Every field but the pid is optional and omitted when absent, matching the
// audio frame dictionaries. A process with neither a bundle id nor a resolvable
// name is still reported: it genuinely held a stream, and dropping it would
// understate what was audible. Filtering belongs to the caller.
char *lumi_audio_processes_json(char **error_message) {
    @autoreleasepool {
        AudioObjectPropertyAddress listAddress = {
            .mSelector = kAudioHardwarePropertyProcessObjectList,
            .mScope = kAudioObjectPropertyScopeGlobal,
            .mElement = kAudioObjectPropertyElementMain,
        };
        UInt32 dataSize = 0;
        OSStatus status = AudioObjectGetPropertyDataSize(kAudioObjectSystemObject, &listAddress,
                                                         0, NULL, &dataSize);
        if (status != noErr) {
            if (error_message != NULL) {
                *error_message = LumiCopyUTF8([NSString stringWithFormat:
                    @"size CoreAudio process object list: status %d", (int)status]);
            }
            return NULL;
        }
        NSMutableArray *processes = [NSMutableArray array];
        UInt32 count = dataSize / (UInt32)sizeof(AudioObjectID);
        if (count > 0) {
            AudioObjectID *objects = calloc(count, sizeof(AudioObjectID));
            if (objects == NULL) {
                if (error_message != NULL) {
                    *error_message = LumiCopyUTF8(@"allocate CoreAudio process object list");
                }
                return NULL;
            }
            status = AudioObjectGetPropertyData(kAudioObjectSystemObject, &listAddress, 0, NULL,
                                                &dataSize, objects);
            if (status != noErr) {
                free(objects);
                if (error_message != NULL) {
                    *error_message = LumiCopyUTF8([NSString stringWithFormat:
                        @"read CoreAudio process object list: status %d", (int)status]);
                }
                return NULL;
            }
            count = dataSize / (UInt32)sizeof(AudioObjectID);
            for (UInt32 index = 0; index < count; index++) {
                AudioObjectID process = objects[index];
                if (!LumiAudioProcessFlag(process, kAudioProcessPropertyIsRunningOutput)) {
                    continue;
                }
                pid_t pid = LumiAudioProcessPID(process);
                NSMutableDictionary *entry = [@{@"pid": @(pid)} mutableCopy];
                NSString *bundleID = LumiAudioProcessBundleID(process);
                if (bundleID != nil) {
                    entry[@"bundle_id"] = bundleID;
                }
                NSRunningApplication *application =
                    [NSRunningApplication runningApplicationWithProcessIdentifier:pid];
                NSString *name = application.localizedName;
                if (name.length > 0) {
                    entry[@"name"] = name;
                }
                [processes addObject:entry];
            }
            free(objects);
        }
        NSError *jsonError = nil;
        NSString *json = LumiJSONString(processes, &jsonError);
        if (json == nil) {
            if (error_message != NULL) *error_message = LumiCopyError(jsonError);
            return NULL;
        }
        return LumiCopyUTF8(json);
    }
}

// LumiLevelMeter accumulates how loud a track is *while it is being captured*,
// so a supervising app can draw a meter that moves with the sound in the room
// rather than once per finished chunk.
//
// It is fed from the ScreenCaptureKit output callback, which is also the path
// every captured sample takes to the writer. That is the whole reason it is
// written the way it is:
//
//   - It never allocates, never logs, never calls Objective-C, and takes its
//     lock for the few nanoseconds it needs to add to two doubles. Capture must
//     not slow down because somebody is looking at a meter.
//   - Anything it does not understand — an unexpected sample format, a buffer it
//     cannot address — is skipped silently. A missing measurement is a bar that
//     does not move; a dropped buffer is lost media, and the two are not
//     comparable. It must fail towards no measurement, always.
//   - Full windows accumulate into a small ring that drops the *oldest* entry
//     when it overflows. A reader that stopped draining is showing a stale
//     meter, and the newest sound is what it wants back when it returns.
//
// Energy leaves here as a mean square of normalised samples, never as decibels.
// The dBFS formula and the silence floor belong to internal/wav and stay there —
// `wav.DBFSFromMeanSquare` is the other half of this — because a second copy in
// Objective-C is exactly the drift the repository forbids, and a language
// boundary would hide it from both test suites.
// LUMI_LEVEL_WINDOWS is the ring's depth: 64 windows, so a caller polling as
// slowly as once a second still loses nothing. The window *length* is not
// defined here on purpose — it is transcript.EnvelopeWindowMS, passed in when
// the session opens, because a second answer to "at what resolution" is the
// drift this file must not introduce.
#define LUMI_LEVEL_WINDOWS 64

typedef struct {
    os_unfair_lock lock;
    // The window still filling.
    double sumSquares;
    int64_t samples;
    int64_t windowSamples;
    // Completed windows, oldest first, as mean squares.
    double windows[LUMI_LEVEL_WINDOWS];
    int count;
    int head;
} LumiLevelMeter;

static void LumiLevelMeterInit(LumiLevelMeter *meter) {
    memset(meter, 0, sizeof(*meter));
    meter->lock = OS_UNFAIR_LOCK_INIT;
}

// LumiLevelMeterPush appends one completed window, dropping the oldest when the
// ring is full. The caller holds the lock.
static void LumiLevelMeterPush(LumiLevelMeter *meter, double meanSquare) {
    int slot = (meter->head + meter->count) % LUMI_LEVEL_WINDOWS;
    meter->windows[slot] = meanSquare;
    if (meter->count < LUMI_LEVEL_WINDOWS) {
        meter->count++;
    } else {
        meter->head = (meter->head + 1) % LUMI_LEVEL_WINDOWS;
    }
}

// LumiLevelMeterAdd folds one buffer's samples in, closing off windows as they
// fill. windowMS is the resolution the measurement is taken at, which the caller
// passes down from transcript.EnvelopeWindowMS so there is one answer to it.
static void LumiLevelMeterAdd(LumiLevelMeter *meter, CMSampleBufferRef sampleBuffer, int windowMS) {
    if (meter == NULL || sampleBuffer == NULL || windowMS <= 0) return;
    CMFormatDescriptionRef format = CMSampleBufferGetFormatDescription(sampleBuffer);
    if (format == NULL) return;
    const AudioStreamBasicDescription *asbd =
        CMAudioFormatDescriptionGetStreamBasicDescription((CMAudioFormatDescriptionRef)format);
    if (asbd == NULL || asbd->mSampleRate <= 0 || asbd->mChannelsPerFrame == 0) return;
    if (asbd->mFormatID != kAudioFormatLinearPCM) return;

    // ScreenCaptureKit does not deliver one format. Measured on macOS 26.5: the
    // system track arrives as non-interleaved 32-bit float at 16kHz, and the
    // microphone as *interleaved 24-bit packed signed integer, stereo, at
    // 48kHz*. An accumulator that assumed float — or assumed the two tracks
    // matched — measured the system track and silently rejected every
    // microphone buffer, which is a meter that never moves for the one source
    // the user is most likely to be testing.
    BOOL isFloat = (asbd->mFormatFlags & kAudioFormatFlagIsFloat) != 0;
    BOOL isSignedInt = (asbd->mFormatFlags & kAudioFormatFlagIsSignedInteger) != 0;
    UInt32 bits = asbd->mBitsPerChannel;
    if (isFloat) {
        if (bits != 32 && bits != 64) return;
    } else if (isSignedInt) {
        // Packed is required because an unpacked sample's bits are not where
        // this arithmetic would look for them.
        if ((asbd->mFormatFlags & kAudioFormatFlagIsPacked) == 0) return;
        if (bits != 16 && bits != 24 && bits != 32) return;
    } else {
        return;
    }

    CMBlockBufferRef block = NULL;
    size_t listSize = 0;
    if (CMSampleBufferGetAudioBufferListWithRetainedBlockBuffer(
            sampleBuffer, &listSize, NULL, 0, NULL, NULL, 0, &block) != noErr) {
        return;
    }
    AudioBufferList *list = (AudioBufferList *)malloc(listSize);
    if (list == NULL) {
        if (block != NULL) CFRelease(block);
        return;
    }
    if (CMSampleBufferGetAudioBufferListWithRetainedBlockBuffer(
            sampleBuffer, &listSize, list, listSize, NULL, NULL, 0, &block) != noErr) {
        free(list);
        if (block != NULL) CFRelease(block);
        return;
    }

    // Every channel folds into one figure. The stored track is a mono downmix,
    // and a meter answers "is sound arriving", not "from which channel".
    // Samples are normalised to -1…1 here so that what crosses back into Go is
    // scale-free, whatever width the hardware chose.
    const double intScale = (bits >= 32) ? 2147483648.0 : (double)(1u << (bits - 1));
    const size_t sampleBytes = bits / 8;
    double sum = 0;
    int64_t counted = 0;
    for (UInt32 i = 0; i < list->mNumberBuffers; i++) {
        const AudioBuffer buffer = list->mBuffers[i];
        if (buffer.mData == NULL || buffer.mDataByteSize == 0) continue;
        const uint8_t *bytes = (const uint8_t *)buffer.mData;
        size_t n = buffer.mDataByteSize / sampleBytes;
        for (size_t sample = 0; sample < n; sample++) {
            const uint8_t *at = bytes + sample * sampleBytes;
            double value = 0;
            if (isFloat) {
                value = (bits == 32) ? (double)(*(const float *)at) : *(const double *)at;
            } else if (bits == 16) {
                value = (double)(*(const int16_t *)at) / intScale;
            } else if (bits == 32) {
                value = (double)(*(const int32_t *)at) / intScale;
            } else {
                // 24-bit packed, little-endian, sign extended by hand: there is
                // no int24_t to load it into.
                int32_t raw = (int32_t)((uint32_t)at[0] | ((uint32_t)at[1] << 8) | ((uint32_t)at[2] << 16));
                if (raw & 0x800000) raw |= ~0xFFFFFF;
                value = (double)raw / intScale;
            }
            sum += value * value;
        }
        counted += (int64_t)n;
    }
    free(list);
    if (block != NULL) CFRelease(block);
    if (counted <= 0) return;

    // Windows are counted in frames, so a window is the same span of time
    // whatever the channel count.
    int64_t windowSamples = (int64_t)llround(asbd->mSampleRate * (double)windowMS / 1000.0) *
                            (int64_t)asbd->mChannelsPerFrame;
    if (windowSamples <= 0) windowSamples = counted;

    os_unfair_lock_lock(&meter->lock);
    meter->windowSamples = windowSamples;
    meter->sumSquares += sum;
    meter->samples += counted;
    // One buffer can complete several windows. Splitting the energy evenly
    // across them is an approximation: within a buffer this coarse the exact
    // sample-by-sample boundary is not worth carrying, and a meter is a readout,
    // not a verdict — the stored file's envelope remains the measured truth.
    while (meter->samples >= meter->windowSamples && meter->windowSamples > 0) {
        double share = meter->sumSquares * (double)meter->windowSamples / (double)meter->samples;
        LumiLevelMeterPush(meter, share / (double)meter->windowSamples);
        meter->sumSquares -= share;
        meter->samples -= meter->windowSamples;
    }
    os_unfair_lock_unlock(&meter->lock);
}

// LumiLevelMeterDrain hands over every completed window and empties the ring, so
// each measurement is reported exactly once. The partial window in progress
// stays behind to finish.
static NSArray<NSNumber *> *LumiLevelMeterDrain(LumiLevelMeter *meter) {
    double drained[LUMI_LEVEL_WINDOWS];
    int count = 0;
    os_unfair_lock_lock(&meter->lock);
    count = meter->count;
    for (int i = 0; i < count; i++) {
        drained[i] = meter->windows[(meter->head + i) % LUMI_LEVEL_WINDOWS];
    }
    meter->count = 0;
    meter->head = 0;
    os_unfair_lock_unlock(&meter->lock);

    NSMutableArray *out = [NSMutableArray arrayWithCapacity:(NSUInteger)count];
    for (int i = 0; i < count; i++) [out addObject:@(drained[i])];
    return out;
}

@interface LumiAudioWriter : NSObject
@property(nonatomic, strong) AVAssetWriter *writer;
@property(nonatomic, strong) AVAssetWriterInput *input;
@property(nonatomic, copy) NSString *path;
@property(nonatomic, assign) BOOL started;
@property(nonatomic, strong) NSError *error;
// sessionStart is the presentation timestamp this writer's session began at.
// Both tracks are fed from one SCStream, so their PTS values share a host
// timebase and the difference between these is the exact skew between the two
// files' t=0 — a value that was previously discarded, leaving cross-track
// timings incomparable.
@property(nonatomic, assign) CMTime sessionStart;
// lastPTSEnd is the end of the most recently appended buffer, so the writer can
// report the span it actually captured rather than the span that was requested.
@property(nonatomic, assign) CMTime lastPTSEnd;
// startedAtUnixNS is the wall clock of the first sample buffer, derived by
// ageing the host clock rather than by sampling NSDate on arrival, so queue
// latency does not accumulate into the anchor.
@property(nonatomic, assign) int64_t startedAtUnixNS;
- (instancetype)initWithPath:(NSString *)path error:(NSError **)error;
- (void)appendSampleBuffer:(CMSampleBufferRef)sampleBuffer;
- (void)finish:(dispatch_group_t)group;
- (int64_t)measuredDurationMS;
- (int64_t)sessionStartPTSNS;
@end

@implementation LumiAudioWriter
- (instancetype)initWithPath:(NSString *)path error:(NSError **)error {
    self = [super init];
    if (self == nil) return nil;
    self.path = path;
    [[NSFileManager defaultManager] removeItemAtPath:path error:nil];
    self.writer = [[AVAssetWriter alloc] initWithURL:[NSURL fileURLWithPath:path]
                                           fileType:AVFileTypeWAVE error:error];
    if (self.writer == nil) return nil;
    NSDictionary *settings = @{
        AVFormatIDKey: @(kAudioFormatLinearPCM),
        AVSampleRateKey: @16000,
        AVNumberOfChannelsKey: @1,
        AVLinearPCMBitDepthKey: @16,
        AVLinearPCMIsFloatKey: @NO,
        AVLinearPCMIsBigEndianKey: @NO,
        AVLinearPCMIsNonInterleaved: @NO,
    };
    self.input = [[AVAssetWriterInput alloc] initWithMediaType:AVMediaTypeAudio outputSettings:settings];
    self.input.expectsMediaDataInRealTime = YES;
    if (![self.writer canAddInput:self.input]) {
        if (error != NULL) {
            *error = [NSError errorWithDomain:@"LumiNative" code:20
                                     userInfo:@{NSLocalizedDescriptionKey: @"configure native WAV writer"}];
        }
        return nil;
    }
    [self.writer addInput:self.input];
    return self;
}

- (void)appendSampleBuffer:(CMSampleBufferRef)sampleBuffer {
    if (self.error != nil || sampleBuffer == NULL || CMSampleBufferGetNumSamples(sampleBuffer) == 0) return;
    CMTime pts = CMSampleBufferGetPresentationTimeStamp(sampleBuffer);
    if (!self.started) {
        if (![self.writer startWriting]) {
            self.error = self.writer.error;
            return;
        }
        [self.writer startSessionAtSourceTime:pts];
        self.sessionStart = pts;
        // Convert this buffer's PTS to wall clock by measuring how old it
        // already is against the same host clock it was stamped from, then
        // subtracting that age from now. Reading NSDate alone would attribute
        // however long the buffer waited on the capture queue to the audio
        // itself, and the whole point of this anchor is that it be tight.
        CMTime hostNow = CMClockGetTime(CMClockGetHostTimeClock());
        Float64 age = CMTimeGetSeconds(CMTimeSubtract(hostNow, pts));
        if (!isfinite(age) || age < 0) age = 0;
        self.startedAtUnixNS = (int64_t)(([[NSDate date] timeIntervalSince1970] - age) * 1e9);
        self.started = YES;
    }
    CMTime duration = CMSampleBufferGetDuration(sampleBuffer);
    CMTime end = CMTIME_IS_NUMERIC(duration) ? CMTimeAdd(pts, duration) : pts;
    if (!CMTIME_IS_NUMERIC(self.lastPTSEnd) || CMTimeCompare(end, self.lastPTSEnd) > 0) {
        self.lastPTSEnd = end;
    }
    if (self.input.readyForMoreMediaData && ![self.input appendSampleBuffer:sampleBuffer]) {
        self.error = self.writer.error;
    }
}

// measuredDurationMS reports the span actually written, or 0 when nothing was.
- (int64_t)measuredDurationMS {
    if (!self.started || !CMTIME_IS_NUMERIC(self.sessionStart) || !CMTIME_IS_NUMERIC(self.lastPTSEnd)) return 0;
    Float64 seconds = CMTimeGetSeconds(CMTimeSubtract(self.lastPTSEnd, self.sessionStart));
    if (!isfinite(seconds) || seconds <= 0) return 0;
    return (int64_t)llround(seconds * 1000.0);
}

// sessionStartPTSNS reports this writer's session start on the shared host
// timebase, or 0 when nothing was written.
- (int64_t)sessionStartPTSNS {
    if (!self.started || !CMTIME_IS_NUMERIC(self.sessionStart)) return 0;
    Float64 seconds = CMTimeGetSeconds(self.sessionStart);
    if (!isfinite(seconds)) return 0;
    return (int64_t)llround(seconds * 1e9);
}

- (void)finish:(dispatch_group_t)group {
	if (!self.started) {
        [self.writer cancelWriting];
        [[NSFileManager defaultManager] removeItemAtPath:self.path error:nil];
        return;
    }
    [self.input markAsFinished];
    dispatch_group_enter(group);
    [self.writer finishWritingWithCompletionHandler:^{
		if (self.writer.status != AVAssetWriterStatusCompleted && self.error == nil) self.error = self.writer.error;
        dispatch_group_leave(group);
    }];
}
@end

// LumiAudioFrameDictionary renders one track's frame. Beyond the requested
// duration every row has always carried, it reports the wall clock of the first
// sample buffer, the session start on the shared host timebase, and the span
// actually captured — the three values that make one track's file-relative
// timings comparable with the other's. Each is omitted when the writer never
// started, so absent and zero stay distinguishable on the Go side.
static NSMutableDictionary *LumiAudioFrameDictionary(NSString *path, NSString *source,
                                                     int64_t requestedDurationMS,
                                                     LumiAudioWriter *writer,
                                                     NSString *captureError) {
    NSMutableDictionary *frame = [@{@"path": path, @"source": source,
                                    @"duration_ms": @(requestedDurationMS)} mutableCopy];
    if (writer.startedAtUnixNS > 0) frame[@"started_at_unix_ns"] = @(writer.startedAtUnixNS);
    int64_t sessionStart = [writer sessionStartPTSNS];
    if (sessionStart != 0) frame[@"session_start_pts_ns"] = @(sessionStart);
    int64_t measured = [writer measuredDurationMS];
    if (measured > 0) frame[@"measured_duration_ms"] = @(measured);
    if (captureError.length > 0) frame[@"capture_error"] = captureError;
    return frame;
}

// LumiAudioSession keeps one SCStream open for an entire recording and slices it
// into chunks by presentation timestamp. Cycling the stream once per chunk —
// start, sleep, stop, finalise the files, start again — left the tap closed for
// roughly two of every thirty-two seconds, and that loss landed mid-sentence.
// Rotation runs on the same serial queue the sample buffers arrive on, so the
// buffer that crosses a boundary opens the next chunk whole instead of falling
// into a gap between two streams. Nothing here sleeps for a chunk's duration:
// the reader collects finished chunks while the stream keeps recording, so a
// slow transcription can no longer cost captured audio either.
@interface LumiAudioSession : NSObject <SCStreamOutput, SCStreamDelegate>
@property(nonatomic, strong) SCStream *stream;
// audioQueue serialises sample delivery and rotation together, which is exactly
// what makes a rotation atomic with respect to the buffers crossing it.
@property(nonatomic, strong) dispatch_queue_t audioQueue;
// finishQueue runs writer finalisation, so closing one chunk never blocks the
// next chunk's samples behind an AVAssetWriter flush.
@property(nonatomic, strong) dispatch_queue_t finishQueue;
// pending counts chunks whose writers have not finished, so the session can
// report itself drained only once every one of them has been handed over.
@property(nonatomic, strong) dispatch_group_t pending;
@property(nonatomic, copy) NSString *directory;
@property(nonatomic, copy) NSString *prefix;
@property(nonatomic, assign) double chunkSeconds;
// levelWindowMS is transcript.EnvelopeWindowMS, handed down by the caller.
@property(nonatomic, assign) int levelWindowMS;
@property(nonatomic, assign) CMTime chunkDuration;
@property(nonatomic, strong) LumiAudioWriter *systemWriter;
@property(nonatomic, strong) LumiAudioWriter *microphoneWriter;
@property(nonatomic, assign) NSUInteger chunkIndex;
@property(nonatomic, assign) BOOL anchored;
@property(nonatomic, assign) CMTime sessionStartPTS;
@property(nonatomic, assign) CMTime nextBoundary;
@property(nonatomic, assign) int64_t sessionStartUnixNS;
@property(nonatomic, assign) int64_t chunkStartUnixNS;
// chunkGridStartUnixNS is where the chunk sits on the drift-free grid;
// chunkStartUnixNS is what was measured at rotation. Keeping both is what lets a
// reader tell clock drift from a mis-stamped chunk.
@property(nonatomic, assign) int64_t chunkGridStartUnixNS;
@property(nonatomic, assign) int64_t chunkStreamOffsetNS;
@property(nonatomic, assign) BOOL chunkClockAnomaly;
@property(atomic, assign) BOOL stopping;
@property(nonatomic, strong) NSCondition *readyCondition;
@property(nonatomic, strong) NSMutableArray *ready;
@property(nonatomic, assign) BOOL drained;
@property(atomic, strong) NSError *streamError;
@end

@implementation LumiAudioSession {
    // One live meter per track, fed from the capture callback and drained by
    // whoever is drawing a meter. Instance variables rather than properties:
    // they are addressed by pointer from a lock-free-ish accumulator and must
    // not be boxed or copied.
    LumiLevelMeter _systemLevels;
    LumiLevelMeter _microphoneLevels;
}

- (instancetype)initWithDirectory:(NSString *)directory
                           prefix:(NSString *)prefix
                     chunkSeconds:(double)chunkSeconds
                    levelWindowMS:(int)levelWindowMS {
    self = [super init];
    if (self == nil) return nil;
    self.directory = directory;
    self.prefix = prefix;
    self.chunkSeconds = MAX(0.1, chunkSeconds);
    self.levelWindowMS = levelWindowMS;
    LumiLevelMeterInit(&_systemLevels);
    LumiLevelMeterInit(&_microphoneLevels);
    // A rational CMTime keeps the boundary exact across thousands of rotations;
    // accumulating a Float64 would drift the chunk grid over a long recording.
    self.chunkDuration = CMTimeMakeWithSeconds(self.chunkSeconds, 90000);
    self.audioQueue = dispatch_queue_create("ai.puremetrics.lumi.audio", DISPATCH_QUEUE_SERIAL);
    self.finishQueue = dispatch_queue_create("ai.puremetrics.lumi.audio.finish", DISPATCH_QUEUE_SERIAL);
    self.pending = dispatch_group_create();
    self.readyCondition = [[NSCondition alloc] init];
    self.ready = [NSMutableArray array];
    return self;
}

- (BOOL)start:(NSError **)error {
    if (@available(macOS 15.0, *)) {
        __block SCShareableContent *content = nil;
        __block NSError *contentError = nil;
        dispatch_semaphore_t contentReady = dispatch_semaphore_create(0);
        [SCShareableContent getShareableContentExcludingDesktopWindows:NO
                                                  onScreenWindowsOnly:NO
                                                    completionHandler:^(SCShareableContent *shareable, NSError *failure) {
            content = shareable;
            contentError = failure;
            dispatch_semaphore_signal(contentReady);
        }];
        if (!LumiWait(contentReady, 10.0, @"enumerate audio capture content", &contentError) ||
            content.displays.count == 0) {
            if (error != NULL) {
                *error = contentError ?: LumiTimeoutError(@"enumerate audio capture content");
            }
            return NO;
        }

        // The first chunk's writers exist before capture starts, so the very
        // first sample buffer has somewhere to land.
        [self openWriters];

        SCContentFilter *filter = [[SCContentFilter alloc] initWithDisplay:content.displays.firstObject
                                                     excludingApplications:@[] exceptingWindows:@[]];
        SCStreamConfiguration *configuration = [[SCStreamConfiguration alloc] init];
        configuration.width = 2;
        configuration.height = 2;
        configuration.minimumFrameInterval = CMTimeMake(1, 1);
        configuration.capturesAudio = YES;
        configuration.sampleRate = 16000;
        configuration.channelCount = 1;
        configuration.excludesCurrentProcessAudio = YES;
        configuration.captureMicrophone = YES;
        self.stream = [[SCStream alloc] initWithFilter:filter configuration:configuration delegate:self];
        NSError *addError = nil;
        if (![self.stream addStreamOutput:self type:SCStreamOutputTypeAudio
                       sampleHandlerQueue:self.audioQueue error:&addError] ||
            ![self.stream addStreamOutput:self type:SCStreamOutputTypeMicrophone
                       sampleHandlerQueue:self.audioQueue error:&addError]) {
            if (error != NULL) *error = addError;
            return NO;
        }

        __block NSError *startError = nil;
        dispatch_semaphore_t started = dispatch_semaphore_create(0);
        [self.stream startCaptureWithCompletionHandler:^(NSError *failure) {
            startError = failure;
            dispatch_semaphore_signal(started);
        }];
        if (!LumiWait(started, 10.0, @"start ScreenCaptureKit audio", &startError) || startError != nil) {
            if (error != NULL) *error = startError ?: LumiTimeoutError(@"start ScreenCaptureKit audio");
            return NO;
        }
        return YES;
    }
    if (error != NULL) {
        *error = [NSError errorWithDomain:@"LumiNative" code:21
                                 userInfo:@{NSLocalizedDescriptionKey:
                                                @"native microphone capture requires macOS 15 or newer"}];
    }
    return NO;
}

#pragma mark - Capture

- (void)stream:(SCStream *)stream didOutputSampleBuffer:(CMSampleBufferRef)sampleBuffer
                                                ofType:(SCStreamOutputType)type {
    BOOL microphone = NO;
    if (type == SCStreamOutputTypeAudio) {
        microphone = NO;
    } else if (@available(macOS 15.0, *)) {
        if (type != SCStreamOutputTypeMicrophone) return;
        microphone = YES;
    } else {
        return;
    }
    if (self.stopping || sampleBuffer == NULL || CMSampleBufferGetNumSamples(sampleBuffer) == 0) return;
    CMTime pts = CMSampleBufferGetPresentationTimeStamp(sampleBuffer);
    if (!CMTIME_IS_NUMERIC(pts)) return;
    if (!self.anchored) [self anchorAtPTS:pts];
    // A buffer straddling a boundary opens the next chunk rather than being
    // split, which puts the boundary within one buffer of exact while keeping
    // every sample on exactly one side of it.
    while (CMTimeCompare(pts, self.nextBoundary) >= 0) [self rotate];
    [(microphone ? self.microphoneWriter : self.systemWriter) appendSampleBuffer:sampleBuffer];
    // After the writer, never before: the media is what must not be lost, and a
    // level is only ever a readout of sound that has already been handed over.
    LumiLevelMeterAdd(microphone ? &_microphoneLevels : &_systemLevels, sampleBuffer,
                      self.levelWindowMS);
}

// drainLevels reports the sound each track has received since the last call.
// Empty arrays are legitimate and mean "nothing completed a window yet", which
// is not the same as silence — silence completes windows too, at the floor.
- (NSDictionary *)drainLevels {
    return @{
        @"window_ms": @(self.levelWindowMS),
        @"system": LumiLevelMeterDrain(&_systemLevels),
        @"microphone": LumiLevelMeterDrain(&_microphoneLevels),
    };
}

- (void)stream:(SCStream *)stream didStopWithError:(NSError *)error {
    if (error != nil) self.streamError = error;
    @synchronized(self) {
        if (self.stopping) return;
        self.stopping = YES;
    }
    // Nothing more will arrive, so close what is open and let the reader see the
    // failure instead of waiting out its timeout against a dead stream.
    dispatch_async(self.audioQueue, ^{
        [self closeCurrentChunk];
        dispatch_group_notify(self.pending, self.finishQueue, ^{ [self markDrained]; });
    });
}

// anchorAtPTS pins the chunk grid to the first buffer that arrives. Wall clock
// comes from ageing the host clock rather than from reading NSDate on arrival,
// so queue latency does not accumulate into the anchor every chunk inherits.
- (void)anchorAtPTS:(CMTime)pts {
    self.sessionStartPTS = pts;
    CMTime hostNow = CMClockGetTime(CMClockGetHostTimeClock());
    Float64 age = CMTimeGetSeconds(CMTimeSubtract(hostNow, pts));
    if (!isfinite(age) || age < 0) age = 0;
    self.sessionStartUnixNS = (int64_t)(([[NSDate date] timeIntervalSince1970] - age) * 1e9);
    // The first chunk needs no measured read: the anchor *is* one, taken the same
    // way rotation takes its own.
    self.chunkStartUnixNS = self.sessionStartUnixNS;
    self.chunkGridStartUnixNS = self.sessionStartUnixNS;
    self.chunkStreamOffsetNS = 0;
    self.chunkClockAnomaly = NO;
    self.nextBoundary = CMTimeAdd(pts, self.chunkDuration);
    self.anchored = YES;
}

// wallClockForPTS places a boundary on the drift-free session grid by offsetting
// the session anchor, so successive grid points are exactly chunkDuration apart.
//
// This used to be the chunk's captured_at, which is why every audio timestamp in
// an index shared one sub-second fraction: the value was arithmetic on a single
// anchor, never an observation, so clock drift was invisible and a dropped chunk
// renumbered silently instead of leaving a hole. It is now the *grid* reference —
// still exported, because coverage arithmetic wants exactness — and the fallback
// whenever a measured read fails a guard below.
- (int64_t)wallClockForPTS:(CMTime)pts {
    if (self.sessionStartUnixNS <= 0) return 0;
    Float64 offset = CMTimeGetSeconds(CMTimeSubtract(pts, self.sessionStartPTS));
    if (!isfinite(offset)) return self.sessionStartUnixNS;
    return self.sessionStartUnixNS + (int64_t)llround(offset * 1e9);
}

// streamOffsetNSForPTS is the boundary's exact distance from the session anchor.
// This is the drift-free quantity captured_at used to carry implicitly; it is now
// stored in its own column so the timestamp can be observed without losing it.
- (int64_t)streamOffsetNSForPTS:(CMTime)pts {
    Float64 offset = CMTimeGetSeconds(CMTimeSubtract(pts, self.sessionStartPTS));
    if (!isfinite(offset) || offset < 0) return 0;
    return (int64_t)llround(offset * 1e9);
}

// measuredWallClockForPTS reads the clock at rotation and ages it back to the
// boundary, rather than offsetting the frozen session anchor.
//
// Ageing is what makes this safe. Reading NSDate on arrival — the obvious version
// — is the bug that made every chunk absorb the previous chunk's processing time,
// leaving chunks 32 s apart while each held 30 s of sound. Subtracting the
// buffer's age puts the value back at the instant the audio actually began, so
// the stamp is genuinely observed while still naming the start of the chunk.
- (int64_t)measuredWallClockForPTS:(CMTime)pts {
    CMTime hostNow = CMClockGetTime(CMClockGetHostTimeClock());
    Float64 age = CMTimeGetSeconds(CMTimeSubtract(hostNow, pts));
    if (!isfinite(age) || age < 0) return 0;
    return (int64_t)(([[NSDate date] timeIntervalSince1970] - age) * 1e9);
}

// LumiMaxMeasuredDriftNS bounds how far a measured chunk start may sit from its
// grid point before the grid value is used instead.
//
// The number comes from the *turn-merge headroom*, not from the chunk duration.
// Chunks sit one chunk duration apart and internal/transcript merges turns across
// a boundary only while the gap stays under DefaultMaxChunkGap (35 s) — so at the
// 30 s default there are just 5 s of slack. A tolerance of half a chunk would
// admit a forward jump of 5-15 s, push the gap past 35 s, and silently split a
// continuously recorded turn mid-sentence: exactly the failure the contiguous
// stream was built to remove. Expected jitter here is sub-millisecond, so 250 ms
// is hundreds of times looser than the signal and twenty times tighter than the
// budget.
static const int64_t LumiMaxMeasuredDriftNS = 250LL * 1000000LL;

// maxMeasuredDriftNS scales the tolerance down for chunk durations shorter than
// it. A fixed 250ms budget is smaller than the turn-merge headroom at the 30s
// default, but --audio-chunk is a flag with no lower bound: at a 100ms chunk an
// accepted 250ms drift exceeds two whole chunks, so a stamp could legitimately
// overtake its successor. Half a chunk is the ceiling that keeps a measured
// stamp inside its own interval.
- (int64_t)maxMeasuredDriftNS {
    int64_t half = (int64_t)(self.chunkSeconds * 0.5 * 1e9);
    return half < LumiMaxMeasuredDriftNS ? half : LumiMaxMeasuredDriftNS;
}

- (void)rotate {
    [self closeCurrentChunk];
    self.chunkIndex += 1;
    int64_t derived = [self wallClockForPTS:self.nextBoundary];
    int64_t measured = [self measuredWallClockForPTS:self.nextBoundary];
    int64_t previous = self.chunkStartUnixNS;
    self.chunkGridStartUnixNS = derived;
    self.chunkStreamOffsetNS = [self streamOffsetNSForPTS:self.nextBoundary];
    self.chunkClockAnomaly = NO;
    if (measured <= 0) {
        measured = derived;
    } else if (measured <= previous) {
        // A wall-clock step backwards (NTP, sleep/wake) must never place a chunk
        // at or before its predecessor: turn continuation requires a strictly
        // positive gap, so this would stop it without reporting anything.
        measured = derived;
        self.chunkClockAnomaly = YES;
    } else if (llabs(measured - derived) > [self maxMeasuredDriftNS]) {
        measured = derived;
        self.chunkClockAnomaly = YES;
    }
    // The fallback itself can be non-increasing: the previous chunk may have kept
    // a measured stamp ahead of its own grid point, and this chunk's grid point
    // sits only one chunk duration later. Falling back is therefore not enough on
    // its own to preserve the strictly-positive gap the guard exists for.
    if (measured <= previous) {
        measured = previous + 1;
        self.chunkClockAnomaly = YES;
    }
    self.chunkStartUnixNS = measured;
    self.nextBoundary = CMTimeAdd(self.nextBoundary, self.chunkDuration);
    [self openWriters];
}

- (NSString *)pathForTrack:(NSString *)track {
    NSString *name = [NSString stringWithFormat:@"%@-%06lu-%@.wav",
                                                self.prefix, (unsigned long)self.chunkIndex, track];
    return [self.directory stringByAppendingPathComponent:name];
}

- (void)openWriters {
    NSError *error = nil;
    self.systemWriter = [[LumiAudioWriter alloc] initWithPath:[self pathForTrack:@"system"] error:&error];
    if (error != nil) self.streamError = self.streamError ?: error;
    error = nil;
    self.microphoneWriter = [[LumiAudioWriter alloc] initWithPath:[self pathForTrack:@"microphone"]
                                                            error:&error];
    if (error != nil) self.streamError = self.streamError ?: error;
}

// closeCurrentChunk hands the open writers off to be finalised and is safe to
// call twice; the second call finds nothing open.
- (void)closeCurrentChunk {
    LumiAudioWriter *system = self.systemWriter;
    LumiAudioWriter *microphone = self.microphoneWriter;
    if (system == nil && microphone == nil) return;
    self.systemWriter = nil;
    self.microphoneWriter = nil;
    // Every stamp is captured before the writers are handed to the async finish
    // queue, so a slow flush cannot contaminate them.
    int64_t startedAtNS = self.chunkStartUnixNS;
    int64_t gridStartedAtNS = self.chunkGridStartUnixNS;
    int64_t streamOffsetNS = self.chunkStreamOffsetNS;
    BOOL clockAnomaly = self.chunkClockAnomaly;
    dispatch_group_enter(self.pending);
    dispatch_group_t writers = dispatch_group_create();
    [system finish:writers];
    [microphone finish:writers];
    dispatch_group_notify(writers, self.finishQueue, ^{
        [self enqueueChunkWithSystem:system
                          microphone:microphone
                         startedAtNS:startedAtNS
                     gridStartedAtNS:gridStartedAtNS
                      streamOffsetNS:streamOffsetNS
                        clockAnomaly:clockAnomaly];
        dispatch_group_leave(self.pending);
    });
}

- (void)enqueueChunkWithSystem:(LumiAudioWriter *)system
                    microphone:(LumiAudioWriter *)microphone
                   startedAtNS:(int64_t)startedAtNS
               gridStartedAtNS:(int64_t)gridStartedAtNS
                streamOffsetNS:(int64_t)streamOffsetNS
                  clockAnomaly:(BOOL)clockAnomaly {
    NSMutableArray *frames = [NSMutableArray array];
    NSError *failure = self.streamError ?: system.error ?: microphone.error;
    NSString *captureError = failure.localizedDescription;
    int64_t requestedMS = (int64_t)llround(self.chunkSeconds * 1000.0);
    NSFileManager *files = [NSFileManager defaultManager];
    if (system != nil && [files fileExistsAtPath:system.path]) {
        [frames addObject:LumiAudioFrameDictionary(system.path, @"system", requestedMS, system, captureError)];
    }
    if (microphone != nil && [files fileExistsAtPath:microphone.path]) {
        [frames addObject:LumiAudioFrameDictionary(microphone.path, @"microphone", requestedMS,
                                                   microphone, captureError)];
    }
    // A chunk that wrote no file at all has no media to index, and reporting it
    // would put an empty pair through attribution as though it were silence.
    if (frames.count == 0) return;
    NSMutableDictionary *chunk = [@{@"frames": frames} mutableCopy];
    if (startedAtNS > 0) chunk[@"started_at_unix_ns"] = @(startedAtNS);
    if (gridStartedAtNS > 0) chunk[@"grid_started_at_unix_ns"] = @(gridStartedAtNS);
    // Written even when zero: the first chunk of every session legitimately has a
    // zero offset, and omitting it there would make "session start" and "not
    // reported" the same value.
    if (self.anchored) chunk[@"stream_offset_ns"] = @(streamOffsetNS);
    if (clockAnomaly) chunk[@"clock_anomaly"] = @(YES);
    [self.readyCondition lock];
    [self.ready addObject:chunk];
    [self.readyCondition broadcast];
    [self.readyCondition unlock];
}

- (void)markDrained {
    [self.readyCondition lock];
    self.drained = YES;
    [self.readyCondition broadcast];
    [self.readyCondition unlock];
}

#pragma mark - Reading

// nextChunkWithTimeout always reports a finished chunk if one is queued, even
// once the session has been stopped: cancellation must not discard audio that
// was already captured and written.
- (NSDictionary *)nextChunkWithTimeout:(NSTimeInterval)seconds {
    NSDate *deadline = [NSDate dateWithTimeIntervalSinceNow:seconds];
    [self.readyCondition lock];
    while (self.ready.count == 0 && !self.drained) {
        if (![self.readyCondition waitUntilDate:deadline]) break;
    }
    NSDictionary *chunk = nil;
    if (self.ready.count > 0) {
        chunk = self.ready.firstObject;
        [self.ready removeObjectAtIndex:0];
    }
    BOOL closed = self.drained && self.ready.count == 0 && chunk == nil;
    [self.readyCondition unlock];
    if (chunk != nil) return chunk;
    if (!closed) return @{@"timeout": @YES};
    NSMutableDictionary *end = [@{@"closed": @YES} mutableCopy];
    NSError *failure = self.streamError;
    if (failure != nil) end[@"capture_error"] = failure.localizedDescription;
    return end;
}

- (void)stop {
    @synchronized(self) {
        if (self.stopping) return;
        self.stopping = YES;
    }
    dispatch_semaphore_t stopped = dispatch_semaphore_create(0);
    [self.stream stopCaptureWithCompletionHandler:^(NSError *error) {
        if (error != nil) self.streamError = error;
        dispatch_semaphore_signal(stopped);
    }];
    NSError *stopTimeout = nil;
    if (!LumiWait(stopped, 10.0, @"stop ScreenCaptureKit audio", &stopTimeout)) {
        self.streamError = self.streamError ?: stopTimeout;
    }
    // Closing on the sample queue guarantees no buffer is still being appended
    // to a writer that is already finishing.
    dispatch_sync(self.audioQueue, ^{ [self closeCurrentChunk]; });
    dispatch_group_notify(self.pending, self.finishQueue, ^{ [self markDrained]; });
}

@end

static NSMutableDictionary<NSNumber *, LumiAudioSession *> *LumiAudioSessionRegistry(void) {
    static NSMutableDictionary *registry = nil;
    static dispatch_once_t once;
    dispatch_once(&once, ^{ registry = [NSMutableDictionary dictionary]; });
    return registry;
}

static LumiAudioSession *LumiAudioSessionForHandle(int64_t handle) {
    NSMutableDictionary *registry = LumiAudioSessionRegistry();
    @synchronized(registry) {
        return registry[@(handle)];
    }
}

// lumi_audio_session_start opens the stream and returns a handle, or 0 with
// *error_message set. Chunks are collected with lumi_audio_session_next_json
// while the stream keeps running.
int64_t lumi_audio_session_start(const char *directory, const char *prefix, double chunk_seconds,
                                 int32_t level_window_ms, char **error_message) {
    @autoreleasepool {
        LumiAudioSession *session =
            [[LumiAudioSession alloc] initWithDirectory:[NSString stringWithUTF8String:directory]
                                                 prefix:[NSString stringWithUTF8String:prefix]
                                           chunkSeconds:chunk_seconds
                                          levelWindowMS:(int)level_window_ms];
        NSError *error = nil;
        if (![session start:&error]) {
            if (error_message != NULL) *error_message = LumiCopyError(error);
            return 0;
        }
        static int64_t nextHandle = 0;
        NSMutableDictionary *registry = LumiAudioSessionRegistry();
        int64_t handle = 0;
        @synchronized(registry) {
            handle = ++nextHandle;
            registry[@(handle)] = session;
        }
        return handle;
    }
}

// lumi_audio_session_next_json waits up to timeout_seconds for the next finished
// chunk. It returns {"frames": [...]} for a chunk, {"timeout": true} when none
// arrived in time, or {"closed": true} once the session has stopped and every
// chunk it captured has been handed over.
char *lumi_audio_session_next_json(int64_t handle, double timeout_seconds, char **error_message) {
    @autoreleasepool {
        LumiAudioSession *session = LumiAudioSessionForHandle(handle);
        if (session == nil) {
            if (error_message != NULL) *error_message = LumiCopyUTF8(@"audio capture session is not open");
            return NULL;
        }
        NSDictionary *chunk = [session nextChunkWithTimeout:MAX(0.0, timeout_seconds)];
        NSError *jsonError = nil;
        NSString *json = LumiJSONString(chunk, &jsonError);
        if (json == nil) {
            if (error_message != NULL) *error_message = LumiCopyError(jsonError);
            return NULL;
        }
        return LumiCopyUTF8(json);
    }
}

// lumi_audio_session_levels_json reports how loud each track has been since the
// last call, as {"window_ms":N,"system":[meanSquare,...],"microphone":[...]}.
//
// Mean squares of normalised samples, not decibels: internal/wav owns the dBFS
// formula and the silence floor, and this must not hold a second copy of either.
// Each window is reported exactly once, so a caller that polls slowly sees every
// measurement rather than a sample of them, up to the ring's depth.
char *lumi_audio_session_levels_json(int64_t handle, char **error_message) {
    @autoreleasepool {
        LumiAudioSession *session = LumiAudioSessionForHandle(handle);
        if (session == nil) {
            if (error_message != NULL) *error_message = LumiCopyUTF8(@"audio capture session is not open");
            return NULL;
        }
        NSError *jsonError = nil;
        NSString *json = LumiJSONString([session drainLevels], &jsonError);
        if (json == nil) {
            if (error_message != NULL) *error_message = LumiCopyError(jsonError);
            return NULL;
        }
        return LumiCopyUTF8(json);
    }
}

// lumi_audio_session_stop stops capture and finalises the chunk in flight. The
// chunks already queued stay readable afterwards, which is what lets a cancelled
// recording keep the audio it had captured up to that instant.
void lumi_audio_session_stop(int64_t handle) {
    @autoreleasepool {
        [LumiAudioSessionForHandle(handle) stop];
    }
}

void lumi_audio_session_close(int64_t handle) {
    @autoreleasepool {
        LumiAudioSession *session = LumiAudioSessionForHandle(handle);
        [session stop];
        NSMutableDictionary *registry = LumiAudioSessionRegistry();
        @synchronized(registry) {
            [registry removeObjectForKey:@(handle)];
        }
    }
}

// MARK: - Media transcoding for `lumi compress`
//
// Three entry points, all pure file-to-file work with no permission surface:
// re-encode a captured JPEG as HEIC, re-encode a captured WAV as lossless FLAC,
// and decode any AVFoundation-readable audio file to mono 16-bit samples.
//
// The two encoders verify by reopening what they wrote rather than trusting the
// encoder's own success return. A truncated or otherwise broken file still
// finalises, and `lumi compress` deletes the source once these report success —
// a silent encoder failure is the one bug in that feature that destroys data.

// LumiImageMeasure compares two decoded images that are already known to share
// dimensions, reporting PSNR and a coarse histogram similarity.
//
// Both are drawn into identical 8-bit RGBA contexts first, because that is the
// only way two images are comparable regardless of the colour spaces they
// decoded into. The histogram uses 16 bins per channel to match the shape
// internal/capture/compare.go uses for frame deduplication, so the two numbers
// stay commensurable; it is deliberately a second, independent signal, because a
// channel-order or colour-space mistake can pass a PSNR check that a histogram
// comparison fails.
static BOOL LumiImageMeasure(CGImageRef source, CGImageRef decoded,
                             double *psnrDB, double *histogramSimilarity, NSError **error) {
    size_t width = CGImageGetWidth(source);
    size_t height = CGImageGetHeight(source);
    if (width == 0 || height == 0) {
        if (error != NULL) {
            *error = [NSError errorWithDomain:@"LumiNative" code:30
                                     userInfo:@{NSLocalizedDescriptionKey: @"image has no pixels"}];
        }
        return NO;
    }
    // Guard the sizing arithmetic before it reaches an allocator: a wrapped
    // byteCount would under-allocate the buffers the bitmap contexts then draw
    // into. No real frame comes near this; an untrusted or corrupt header can.
    if (width > SIZE_MAX / 4 || height > SIZE_MAX / (width * 4)) {
        if (error != NULL) {
            *error = [NSError errorWithDomain:@"LumiNative" code:35
                                     userInfo:@{NSLocalizedDescriptionKey: @"image dimensions are too large to compare"}];
        }
        return NO;
    }
    size_t bytesPerRow = width * 4;
    size_t byteCount = bytesPerRow * height;

    CGColorSpaceRef space = CGColorSpaceCreateDeviceRGB();
    if (space == NULL) {
        if (error != NULL) {
            *error = [NSError errorWithDomain:@"LumiNative" code:36
                                     userInfo:@{NSLocalizedDescriptionKey: @"create comparison colour space"}];
        }
        return NO;
    }
    uint8_t *sourcePixels = calloc(byteCount, 1);
    uint8_t *decodedPixels = calloc(byteCount, 1);
    CGContextRef sourceContext = NULL;
    CGContextRef decodedContext = NULL;
    BOOL ok = NO;

    if (sourcePixels != NULL && decodedPixels != NULL) {
        sourceContext = CGBitmapContextCreate(sourcePixels, width, height, 8, bytesPerRow, space,
                                              (CGBitmapInfo)kCGImageAlphaPremultipliedLast);
        decodedContext = CGBitmapContextCreate(decodedPixels, width, height, 8, bytesPerRow, space,
                                               (CGBitmapInfo)kCGImageAlphaPremultipliedLast);
    }
    if (sourceContext != NULL && decodedContext != NULL) {
        CGRect rect = CGRectMake(0, 0, (CGFloat)width, (CGFloat)height);
        CGContextDrawImage(sourceContext, rect, source);
        CGContextDrawImage(decodedContext, rect, decoded);

        double squaredError = 0.0;
        double sourceBins[48] = {0};
        double decodedBins[48] = {0};
        for (size_t i = 0; i < byteCount; i += 4) {
            for (size_t channel = 0; channel < 3; channel++) {
                double a = sourcePixels[i + channel];
                double b = decodedPixels[i + channel];
                squaredError += (a - b) * (a - b);
                sourceBins[channel * 16 + (sourcePixels[i + channel] >> 4)] += 1.0;
                decodedBins[channel * 16 + (decodedPixels[i + channel] >> 4)] += 1.0;
            }
        }
        double samples = (double)width * (double)height * 3.0;
        double mse = squaredError / samples;
        // An identical re-encode has no error at all; 99 dB stands in for the
        // infinity so the value stays finite and JSON-encodable.
        *psnrDB = mse == 0.0 ? 99.0 : MIN(99.0, 10.0 * log10((255.0 * 255.0) / mse));

        double intersection = 0.0;
        double total = 0.0;
        for (size_t bin = 0; bin < 48; bin++) {
            intersection += MIN(sourceBins[bin], decodedBins[bin]);
            total += sourceBins[bin];
        }
        *histogramSimilarity = total == 0.0 ? 1.0 : intersection / total;
        ok = YES;
    } else if (error != NULL) {
        *error = [NSError errorWithDomain:@"LumiNative" code:31
                                 userInfo:@{NSLocalizedDescriptionKey: @"allocate comparison bitmaps"}];
    }

    if (sourceContext != NULL) CGContextRelease(sourceContext);
    if (decodedContext != NULL) CGContextRelease(decodedContext);
    free(sourcePixels);
    free(decodedPixels);
    CGColorSpaceRelease(space);
    return ok;
}

static CGImageRef LumiLoadImage(NSString *path, NSError **error) CF_RETURNS_RETAINED {
    NSURL *url = [NSURL fileURLWithPath:path];
    CGImageSourceRef source = CGImageSourceCreateWithURL((__bridge CFURLRef)url, NULL);
    if (source == NULL) {
        if (error != NULL) {
            *error = [NSError errorWithDomain:@"LumiNative" code:32
                                     userInfo:@{NSLocalizedDescriptionKey:
                                                    [NSString stringWithFormat:@"open image %@", path]}];
        }
        return NULL;
    }
    CGImageSourceStatus status = CGImageSourceGetStatusAtIndex(source, 0);
    CGImageRef image = CGImageSourceCreateImageAtIndex(source, 0, NULL);
    CFRelease(source);
    if (status != kCGImageStatusComplete) {
        BOOL producedPixels = image != NULL;
        if (image != NULL) CGImageRelease(image);
        if (error != NULL) {
            NSString *detail = producedPixels ? @"; decoder still produced pixels" : @"";
            *error = [NSError errorWithDomain:@"LumiNative" code:33
                                     userInfo:@{NSLocalizedDescriptionKey:
                                                    [NSString stringWithFormat:
                                                         @"incomplete image %@ (ImageIO status %ld%@)",
                                                         path, (long)status, detail]}];
        }
        return NULL;
    }
    if (image == NULL && error != NULL) {
        *error = [NSError errorWithDomain:@"LumiNative" code:33
                                 userInfo:@{NSLocalizedDescriptionKey:
                                                [NSString stringWithFormat:@"decode image %@", path]}];
    }
    return image;
}

static long long LumiFileSize(NSString *path) {
    NSDictionary *attributes = [NSFileManager.defaultManager attributesOfItemAtPath:path error:nil];
    return attributes == nil ? 0 : (long long)[attributes fileSize];
}

// lumi_image_inspect_json reports what an existing image decodes to, without
// re-encoding anything. `lumi compress` uses it to decide whether a compressed
// file left behind by an interrupted run is intact enough to adopt, in the one
// case where the original it would have been compared against is already gone.
//
// It requires ImageIO to report the indexed image complete and then draws it
// rather than trusting its header. A truncated file can keep an intact header
// and even produce pixels; neither is enough to adopt it as an event's only
// surviving media.
char *lumi_image_inspect_json(const char *image_path, char **error_message) {
    @autoreleasepool {
        NSString *path = @(image_path);
        NSError *error = nil;
        CGImageRef image = LumiLoadImage(path, &error);
        if (image == NULL) {
            if (error_message != NULL) *error_message = LumiCopyError(error);
            return NULL;
        }
        // Compared against itself: the comparison is what forces every pixel
        // through a bitmap context, and an image that cannot be drawn fails here
        // rather than being reported as intact.
        double psnr = 0.0;
        double histogram = 0.0;
        if (!LumiImageMeasure(image, image, &psnr, &histogram, &error)) {
            CGImageRelease(image);
            if (error_message != NULL) *error_message = LumiCopyError(error);
            return NULL;
        }
        NSDictionary *report = @{
            @"width": @((long long)CGImageGetWidth(image)),
            @"height": @((long long)CGImageGetHeight(image)),
            @"source_width": @((long long)CGImageGetWidth(image)),
            @"source_height": @((long long)CGImageGetHeight(image)),
            @"psnr_db": @(psnr),
            @"histogram_similarity": @(histogram),
            @"bytes": @(LumiFileSize(path)),
        };
        CGImageRelease(image);
        NSError *jsonError = nil;
        NSString *json = LumiJSONString(report, &jsonError);
        if (json == nil) {
            if (error_message != NULL) *error_message = LumiCopyError(jsonError);
            return NULL;
        }
        return LumiCopyUTF8(json);
    }
}

// lumi_image_transcode_heic_json re-encodes an image as HEIC and measures the
// result against its source.
//
// The measurement decodes the file that was actually written, not the CGImage
// that was handed to the encoder, because CGImageDestinationFinalize reporting
// success does not establish that the bytes on disk decode to the right picture.
char *lumi_image_transcode_heic_json(const char *source_path, const char *destination_path,
                                     double quality, char **error_message) {
    @autoreleasepool {
        NSString *sourcePath = @(source_path);
        NSString *destinationPath = @(destination_path);
        NSError *error = nil;

        CGImageRef source = LumiLoadImage(sourcePath, &error);
        if (source == NULL) {
            if (error_message != NULL) *error_message = LumiCopyError(error);
            return NULL;
        }

        NSURL *destinationURL = [NSURL fileURLWithPath:destinationPath];
        CGImageDestinationRef destination = CGImageDestinationCreateWithURL(
            (__bridge CFURLRef)destinationURL, CFSTR("public.heic"), 1, NULL);
        if (destination == NULL) {
            CGImageRelease(source);
            if (error_message != NULL) {
                *error_message = LumiCopyUTF8(@"create HEIC destination");
            }
            return NULL;
        }
        NSDictionary *properties = @{
            (__bridge NSString *)kCGImageDestinationLossyCompressionQuality: @(quality),
        };
        CGImageDestinationAddImage(destination, source, (__bridge CFDictionaryRef)properties);
        BOOL finalized = CGImageDestinationFinalize(destination);
        CFRelease(destination);
        if (!finalized) {
            CGImageRelease(source);
            if (error_message != NULL) *error_message = LumiCopyUTF8(@"finalize HEIC image");
            return NULL;
        }

        CGImageRef decoded = LumiLoadImage(destinationPath, &error);
        if (decoded == NULL) {
            CGImageRelease(source);
            if (error_message != NULL) *error_message = LumiCopyError(error);
            return NULL;
        }

        double psnr = 0.0;
        double histogram = 0.0;
        BOOL sameSize = CGImageGetWidth(decoded) == CGImageGetWidth(source) &&
                        CGImageGetHeight(decoded) == CGImageGetHeight(source);
        // Measuring across differing dimensions would scale one image into the
        // other's grid and report a similarity for a picture neither file holds.
        // The size mismatch is itself the finding; the caller rejects on it.
        if (sameSize && !LumiImageMeasure(source, decoded, &psnr, &histogram, &error)) {
            CGImageRelease(source);
            CGImageRelease(decoded);
            if (error_message != NULL) *error_message = LumiCopyError(error);
            return NULL;
        }

        NSDictionary *report = @{
            @"width": @((long long)CGImageGetWidth(decoded)),
            @"height": @((long long)CGImageGetHeight(decoded)),
            @"source_width": @((long long)CGImageGetWidth(source)),
            @"source_height": @((long long)CGImageGetHeight(source)),
            @"psnr_db": @(psnr),
            @"histogram_similarity": @(histogram),
            @"bytes": @(LumiFileSize(destinationPath)),
        };
        CGImageRelease(source);
        CGImageRelease(decoded);

        NSError *jsonError = nil;
        NSString *json = LumiJSONString(report, &jsonError);
        if (json == nil) {
            if (error_message != NULL) *error_message = LumiCopyError(jsonError);
            return NULL;
        }
        return LumiCopyUTF8(json);
    }
}

// LumiCopyAudioSamples streams every frame of `input` into `output`.
//
// Reads are bounded by the file's own length rather than run until a zero-length
// read: AVAudioFile raises past the end of some compressed files instead of
// reporting zero frames, which turned a complete copy into a failure.
static BOOL LumiCopyAudioSamples(AVAudioFile *input, AVAudioFile *output, NSError **error) {
    const AVAudioFrameCount chunkFrames = 65536;
    AVAudioPCMBuffer *buffer = [[AVAudioPCMBuffer alloc] initWithPCMFormat:input.processingFormat
                                                            frameCapacity:chunkFrames];
    if (buffer == nil) {
        if (error != NULL) {
            *error = [NSError errorWithDomain:@"LumiNative" code:34
                                     userInfo:@{NSLocalizedDescriptionKey: @"allocate audio buffer"}];
        }
        return NO;
    }
    AVAudioFramePosition remaining = input.length;
    while (remaining > 0) {
        AVAudioFrameCount want = (AVAudioFrameCount)MIN((AVAudioFramePosition)chunkFrames, remaining);
        if (![input readIntoBuffer:buffer frameCount:want error:error]) {
            return NO;
        }
        if (buffer.frameLength == 0) {
            break;
        }
        if (output != nil && ![output writeFromBuffer:buffer error:error]) {
            return NO;
        }
        remaining -= (AVAudioFramePosition)buffer.frameLength;
    }
    return YES;
}

// lumi_audio_encode_flac_json re-encodes an audio file as lossless FLAC.
//
// The target format is built from an explicit AudioStreamBasicDescription rather
// than from a settings dictionary, because FLAC carries its source bit depth in
// mFormatFlags and there is no AVAudioFile settings key that reaches it. Passing
// AVFormatIDKey alone yields a format reported as "UNKNOWN source bit depth"
// whose first write fails; AVEncoderBitDepthHintKey does not fill it in either.
//
// The writer is closed before the file is measured, because AVAudioFile finalises
// its header on deallocation — reading it any earlier sees a header and no audio.
char *lumi_audio_encode_flac_json(const char *source_path, const char *destination_path,
                                  char **error_message) {
    @autoreleasepool {
        NSString *sourcePath = @(source_path);
        NSString *destinationPath = @(destination_path);
        NSError *error = nil;
        AVAudioFramePosition frames = 0;
        double sampleRate = 0.0;

        @autoreleasepool {
            AVAudioFile *input = [[AVAudioFile alloc] initForReading:[NSURL fileURLWithPath:sourcePath]
                                                       commonFormat:AVAudioPCMFormatInt16
                                                        interleaved:YES
                                                              error:&error];
            if (input == nil) {
                if (error_message != NULL) *error_message = LumiCopyError(error);
                return NULL;
            }
            frames = input.length;
            sampleRate = input.fileFormat.sampleRate;

            AudioStreamBasicDescription description = {0};
            description.mSampleRate = input.fileFormat.sampleRate;
            description.mFormatID = kAudioFormatFLAC;
            description.mFormatFlags = 16; // source bit depth; Lumi captures 16-bit PCM
            description.mChannelsPerFrame = input.fileFormat.channelCount;
            AVAudioFormat *target = [[AVAudioFormat alloc] initWithStreamDescription:&description];
            if (target == nil) {
                if (error_message != NULL) *error_message = LumiCopyUTF8(@"build FLAC output format");
                return NULL;
            }

            [NSFileManager.defaultManager removeItemAtPath:destinationPath error:nil];
            AVAudioFile *output = [[AVAudioFile alloc]
                initForWriting:[NSURL fileURLWithPath:destinationPath]
                      settings:target.settings
                  commonFormat:AVAudioPCMFormatInt16
                   interleaved:YES
                         error:&error];
            if (output == nil) {
                if (error_message != NULL) *error_message = LumiCopyError(error);
                return NULL;
            }
            if (!LumiCopyAudioSamples(input, output, &error)) {
                if (error_message != NULL) *error_message = LumiCopyError(error);
                return NULL;
            }
        }
        // Both files are closed here, so the FLAC on disk is complete.

        NSDictionary *report = @{
            @"bytes": @(LumiFileSize(destinationPath)),
            @"frames": @((long long)frames),
            @"sample_rate": @((long long)llround(sampleRate)),
        };
        NSError *jsonError = nil;
        NSString *json = LumiJSONString(report, &jsonError);
        if (json == nil) {
            if (error_message != NULL) *error_message = LumiCopyError(jsonError);
            return NULL;
        }
        return LumiCopyUTF8(json);
    }
}

// lumi_audio_decode_pcm16 decodes any file AVFoundation can open into mono
// 16-bit little-endian samples at the file's own sample rate.
//
// It is the only bridge entry point returning a raw buffer rather than a C
// string: a 30-second chunk is 480,000 samples, which no string encoding of the
// data would survive. The caller owns the returned pointer and frees it with
// free(); NULL means failure and *error_message then holds the reason.
//
// Asking AVAudioFile for AVAudioPCMFormatInt16 directly, rather than reading
// float samples and scaling them here, is what makes a lossless round trip
// bit-exact — which is what lets `lumi compress` verify a FLAC by comparison
// rather than by tolerance.
uint8_t *lumi_audio_decode_pcm16(const char *path, int64_t *frames, int32_t *sample_rate,
                                 char **error_message) {
    @autoreleasepool {
        NSError *error = nil;
        AVAudioFile *input = [[AVAudioFile alloc] initForReading:[NSURL fileURLWithPath:@(path)]
                                                   commonFormat:AVAudioPCMFormatInt16
                                                    interleaved:YES
                                                          error:&error];
        if (input == nil) {
            if (error_message != NULL) *error_message = LumiCopyError(error);
            return NULL;
        }
        AVAudioFramePosition total = input.length;
        AVAudioChannelCount channels = input.processingFormat.channelCount;
        if (total < 0 || channels == 0) {
            if (error_message != NULL) *error_message = LumiCopyUTF8(@"audio file reports no frames");
            return NULL;
        }
        if (total > (AVAudioFramePosition)(SIZE_MAX / sizeof(int16_t))) {
            if (error_message != NULL) *error_message = LumiCopyUTF8(@"audio file is too large to decode");
            return NULL;
        }

        int16_t *samples = calloc((size_t)MAX((AVAudioFramePosition)1, total), sizeof(int16_t));
        if (samples == NULL) {
            if (error_message != NULL) *error_message = LumiCopyUTF8(@"allocate decoded samples");
            return NULL;
        }

        const AVAudioFrameCount chunkFrames = 65536;
        AVAudioPCMBuffer *buffer = [[AVAudioPCMBuffer alloc] initWithPCMFormat:input.processingFormat
                                                                frameCapacity:chunkFrames];
        if (buffer == nil) {
            free(samples);
            if (error_message != NULL) *error_message = LumiCopyUTF8(@"allocate audio buffer");
            return NULL;
        }

        AVAudioFramePosition written = 0;
        AVAudioFramePosition remaining = total;
        while (remaining > 0) {
            AVAudioFrameCount want = (AVAudioFrameCount)MIN((AVAudioFramePosition)chunkFrames, remaining);
            if (![input readIntoBuffer:buffer frameCount:want error:&error]) {
                free(samples);
                if (error_message != NULL) *error_message = LumiCopyError(error);
                return NULL;
            }
            if (buffer.frameLength == 0) {
                break;
            }
            const int16_t *source = buffer.int16ChannelData[0];
            for (AVAudioFrameCount frame = 0; frame < buffer.frameLength; frame++) {
                if (channels == 1) {
                    samples[written + frame] = source[frame];
                } else {
                    // Lumi records mono, so this only runs for a file handed in
                    // from outside. Averaging keeps a stereo track's energy
                    // comparable to a mono one rather than discarding a channel.
                    int32_t sum = 0;
                    for (AVAudioChannelCount channel = 0; channel < channels; channel++) {
                        sum += source[frame * channels + channel];
                    }
                    samples[written + frame] = (int16_t)(sum / (int32_t)channels);
                }
            }
            written += (AVAudioFramePosition)buffer.frameLength;
            remaining -= (AVAudioFramePosition)buffer.frameLength;
        }

        if (frames != NULL) *frames = (int64_t)written;
        if (sample_rate != NULL) *sample_rate = (int32_t)llround(input.fileFormat.sampleRate);
        return (uint8_t *)samples;
    }
}
