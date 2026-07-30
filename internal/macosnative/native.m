#import <AppKit/AppKit.h>
#import <ApplicationServices/ApplicationServices.h>
#import <AVFoundation/AVFoundation.h>
#import <AudioToolbox/AudioToolbox.h>
#import <CoreGraphics/CoreGraphics.h>
#import <ImageIO/ImageIO.h>
#import <IOKit/hidsystem/IOHIDLib.h>
#import <ScreenCaptureKit/ScreenCaptureKit.h>
#import <Speech/Speech.h>
#import <Vision/Vision.h>

#include <stdlib.h>
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

char *lumi_capture_screens_json(const char *directory, const char *prefix, char **error_message) {
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
        for (SCDisplay *display in content.displays) {
            @autoreleasepool {
                SCContentFilter *filter = [[SCContentFilter alloc] initWithDisplay:display
                                                             excludingApplications:@[]
                                                                  exceptingWindows:@[]];
                SCShareableContentInfo *info = [SCShareableContent infoForFilter:filter];
                CGFloat scale = MAX(1.0, info.pointPixelScale);
                SCStreamConfiguration *configuration = [[SCStreamConfiguration alloc] init];
                configuration.width = (size_t)llround((double)display.width * scale);
                configuration.height = (size_t)llround((double)display.height * scale);
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
@property(nonatomic, assign) CMTime chunkDuration;
@property(nonatomic, strong) LumiAudioWriter *systemWriter;
@property(nonatomic, strong) LumiAudioWriter *microphoneWriter;
@property(nonatomic, assign) NSUInteger chunkIndex;
@property(nonatomic, assign) BOOL anchored;
@property(nonatomic, assign) CMTime sessionStartPTS;
@property(nonatomic, assign) CMTime nextBoundary;
@property(nonatomic, assign) int64_t sessionStartUnixNS;
@property(nonatomic, assign) int64_t chunkStartUnixNS;
@property(atomic, assign) BOOL stopping;
@property(nonatomic, strong) NSCondition *readyCondition;
@property(nonatomic, strong) NSMutableArray *ready;
@property(nonatomic, assign) BOOL drained;
@property(atomic, strong) NSError *streamError;
@end

@implementation LumiAudioSession

- (instancetype)initWithDirectory:(NSString *)directory
                           prefix:(NSString *)prefix
                     chunkSeconds:(double)chunkSeconds {
    self = [super init];
    if (self == nil) return nil;
    self.directory = directory;
    self.prefix = prefix;
    self.chunkSeconds = MAX(0.1, chunkSeconds);
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
    self.chunkStartUnixNS = self.sessionStartUnixNS;
    self.nextBoundary = CMTimeAdd(pts, self.chunkDuration);
    self.anchored = YES;
}

// wallClockForPTS places a boundary on the wall clock by offsetting the session
// anchor, so successive chunks are exactly chunkDuration apart rather than
// however long the previous rotation happened to take.
- (int64_t)wallClockForPTS:(CMTime)pts {
    if (self.sessionStartUnixNS <= 0) return 0;
    Float64 offset = CMTimeGetSeconds(CMTimeSubtract(pts, self.sessionStartPTS));
    if (!isfinite(offset)) return self.sessionStartUnixNS;
    return self.sessionStartUnixNS + (int64_t)llround(offset * 1e9);
}

- (void)rotate {
    [self closeCurrentChunk];
    self.chunkIndex += 1;
    self.chunkStartUnixNS = [self wallClockForPTS:self.nextBoundary];
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
    int64_t startedAtNS = self.chunkStartUnixNS;
    dispatch_group_enter(self.pending);
    dispatch_group_t writers = dispatch_group_create();
    [system finish:writers];
    [microphone finish:writers];
    dispatch_group_notify(writers, self.finishQueue, ^{
        [self enqueueChunkWithSystem:system microphone:microphone startedAtNS:startedAtNS];
        dispatch_group_leave(self.pending);
    });
}

- (void)enqueueChunkWithSystem:(LumiAudioWriter *)system
                    microphone:(LumiAudioWriter *)microphone
                   startedAtNS:(int64_t)startedAtNS {
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
                                 char **error_message) {
    @autoreleasepool {
        LumiAudioSession *session =
            [[LumiAudioSession alloc] initWithDirectory:[NSString stringWithUTF8String:directory]
                                                 prefix:[NSString stringWithUTF8String:prefix]
                                           chunkSeconds:chunk_seconds];
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
