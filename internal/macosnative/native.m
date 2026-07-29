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
static NSString *const LumiAppSourceWindowList = @"window_list";
static NSString *const LumiAppSourceWorkspace = @"workspace";
static NSString *const LumiAppSourceRunningApplication = @"running_application";

// LumiActivationPID asks the system-wide Accessibility element which application
// holds focus. This is the only *activation* source that is both live and
// correct for an application with no on-screen window — a Finder with every
// window closed, or an app whose windows are all minimized — where the window
// list necessarily reports whichever app is visually behind it.
//
// It returns 0 rather than an error because it is genuinely unreliable: it needs
// Accessibility trust, and it fails per-application (observed returning
// kAXErrorNotImplemented for some Electron apps while succeeding for others in
// the same session). Callers fall through to the window list when it does.
static pid_t LumiActivationPID(void) {
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

// LumiResolveFrontmostLive completes LumiResolveFrontmost against the frameworks
// it deliberately does not touch. The pure function can name a pid it cannot
// name an app for; only here may that be repaired.
static NSDictionary *LumiResolveFrontmostLive(NSArray *windows, pid_t activePID,
                                              NSRunningApplication *frontmost) {
    pid_t workspacePID = frontmost == nil ? 0 : frontmost.processIdentifier;
    NSString *workspaceName = frontmost.localizedName ?: @"";
    NSMutableDictionary *resolved =
        [LumiResolveFrontmost(windows, activePID, workspacePID, workspaceName, getpid()) mutableCopy];
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
        NSDictionary *resolved = LumiResolveFrontmostLive(
            windows, LumiActivationPID(), NSWorkspace.sharedWorkspace.frontmostApplication);
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
        pid_t activePID = LumiActivationPID();
        NSString *activeName = activePID == 0 ? @"" :
            ([NSRunningApplication runningApplicationWithProcessIdentifier:activePID].localizedName ?: @"");
        NSDictionary *resolved = LumiResolveFrontmostLive(windows, activePID, frontmost);
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
- (instancetype)initWithPath:(NSString *)path error:(NSError **)error;
- (void)appendSampleBuffer:(CMSampleBufferRef)sampleBuffer;
- (void)finish:(dispatch_group_t)group;
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
    if (!self.started) {
        if (![self.writer startWriting]) {
            self.error = self.writer.error;
            return;
        }
        [self.writer startSessionAtSourceTime:CMSampleBufferGetPresentationTimeStamp(sampleBuffer)];
        self.started = YES;
    }
    if (self.input.readyForMoreMediaData && ![self.input appendSampleBuffer:sampleBuffer]) {
        self.error = self.writer.error;
    }
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

@interface LumiAudioCapture : NSObject <SCStreamOutput, SCStreamDelegate>
@property(nonatomic, strong) LumiAudioWriter *systemWriter;
@property(nonatomic, strong) LumiAudioWriter *microphoneWriter;
@property(nonatomic, strong) NSError *streamError;
@end

@implementation LumiAudioCapture
- (void)stream:(SCStream *)stream didOutputSampleBuffer:(CMSampleBufferRef)sampleBuffer
                                                ofType:(SCStreamOutputType)type {
    if (type == SCStreamOutputTypeAudio) {
        [self.systemWriter appendSampleBuffer:sampleBuffer];
    } else if (@available(macOS 15.0, *)) {
        if (type == SCStreamOutputTypeMicrophone) [self.microphoneWriter appendSampleBuffer:sampleBuffer];
    }
}

- (void)stream:(SCStream *)stream didStopWithError:(NSError *)error {
    self.streamError = error;
}
@end

char *lumi_record_audio_json(const char *directory, const char *prefix, double duration_seconds,
                             char **error_message) {
    @autoreleasepool {
        if (@available(macOS 15.0, *)) {
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
            if (!LumiWait(contentReady, 10.0, @"enumerate audio capture content", &contentError)) {
                if (error_message != NULL) *error_message = LumiCopyError(contentError);
                return NULL;
            }
            if (content.displays.count == 0) {
                if (error_message != NULL) *error_message = LumiCopyError(contentError);
                return NULL;
            }

            NSString *directoryPath = [NSString stringWithUTF8String:directory];
            NSString *filePrefix = [NSString stringWithUTF8String:prefix];
            NSString *systemPath = [directoryPath stringByAppendingPathComponent:
                                    [filePrefix stringByAppendingString:@"-system.wav"]];
            NSString *microphonePath = [directoryPath stringByAppendingPathComponent:
                                        [filePrefix stringByAppendingString:@"-microphone.wav"]];
            NSError *writerError = nil;
            LumiAudioCapture *output = [[LumiAudioCapture alloc] init];
            output.systemWriter = [[LumiAudioWriter alloc] initWithPath:systemPath error:&writerError];
            if (output.systemWriter == nil) {
                if (error_message != NULL) *error_message = LumiCopyError(writerError);
                return NULL;
            }
            output.microphoneWriter = [[LumiAudioWriter alloc] initWithPath:microphonePath error:&writerError];
            if (output.microphoneWriter == nil) {
                if (error_message != NULL) *error_message = LumiCopyError(writerError);
                return NULL;
            }

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
            SCStream *stream = [[SCStream alloc] initWithFilter:filter configuration:configuration delegate:output];
            dispatch_queue_t audioQueue = dispatch_queue_create("ai.puremetrics.lumi.audio", DISPATCH_QUEUE_SERIAL);
            NSError *addError = nil;
            if (![stream addStreamOutput:output type:SCStreamOutputTypeAudio sampleHandlerQueue:audioQueue error:&addError] ||
                ![stream addStreamOutput:output type:SCStreamOutputTypeMicrophone sampleHandlerQueue:audioQueue error:&addError]) {
                if (error_message != NULL) *error_message = LumiCopyError(addError);
                return NULL;
            }

            __block NSError *startError = nil;
            dispatch_semaphore_t started = dispatch_semaphore_create(0);
            [stream startCaptureWithCompletionHandler:^(NSError *error) {
                startError = error;
                dispatch_semaphore_signal(started);
            }];
            if (!LumiWait(started, 10.0, @"start ScreenCaptureKit audio", &startError)) {
                if (error_message != NULL) *error_message = LumiCopyError(startError);
                return NULL;
            }
            if (startError != nil) {
                if (error_message != NULL) *error_message = LumiCopyError(startError);
                return NULL;
            }
            [NSThread sleepForTimeInterval:MAX(0.1, duration_seconds)];
            dispatch_semaphore_t stopped = dispatch_semaphore_create(0);
            [stream stopCaptureWithCompletionHandler:^(NSError *error) {
                if (error != nil) output.streamError = error;
                dispatch_semaphore_signal(stopped);
            }];
            NSError *stopTimeout = nil;
            if (!LumiWait(stopped, 10.0, @"stop ScreenCaptureKit audio", &stopTimeout)) {
                output.streamError = stopTimeout;
            }
            dispatch_semaphore_t audioDrained = dispatch_semaphore_create(0);
            dispatch_async(audioQueue, ^{ dispatch_semaphore_signal(audioDrained); });
            NSError *drainTimeout = nil;
            if (!LumiWait(audioDrained, 5.0, @"drain ScreenCaptureKit audio", &drainTimeout)) {
                output.streamError = output.streamError ?: drainTimeout;
            }
            dispatch_group_t writersFinished = dispatch_group_create();
            [output.systemWriter finish:writersFinished];
            [output.microphoneWriter finish:writersFinished];
            if (dispatch_group_wait(writersFinished,
                                    dispatch_time(DISPATCH_TIME_NOW, 10 * NSEC_PER_SEC)) != 0) {
                output.streamError = output.streamError ?: LumiTimeoutError(@"finalize native WAV files");
            }

            NSMutableArray *frames = [NSMutableArray array];
            int64_t durationMS = (int64_t)llround(MAX(0.1, duration_seconds) * 1000.0);
            NSError *finalError = output.streamError ?: output.systemWriter.error ?: output.microphoneWriter.error;
            NSString *captureError = finalError.localizedDescription;
            if ([[NSFileManager defaultManager] fileExistsAtPath:systemPath]) {
				NSMutableDictionary *frame = [@{@"path": systemPath, @"source": @"system",
				                                  @"duration_ms": @(durationMS)} mutableCopy];
				if (captureError.length > 0) frame[@"capture_error"] = captureError;
				[frames addObject:frame];
            }
            if ([[NSFileManager defaultManager] fileExistsAtPath:microphonePath]) {
				NSMutableDictionary *frame = [@{@"path": microphonePath, @"source": @"microphone",
				                                  @"duration_ms": @(durationMS)} mutableCopy];
				if (captureError.length > 0) frame[@"capture_error"] = captureError;
				[frames addObject:frame];
			}
			if (frames.count == 0 && finalError != nil) {
				if (error_message != NULL) *error_message = LumiCopyError(finalError);
				return NULL;
            }
            NSError *jsonError = nil;
            NSString *json = LumiJSONString(frames, &jsonError);
            if (json == nil) {
                if (error_message != NULL) *error_message = LumiCopyError(jsonError);
                return NULL;
            }
            return LumiCopyUTF8(json);
        }
        if (error_message != NULL) {
            *error_message = LumiCopyUTF8(@"native microphone capture requires macOS 15 or newer");
        }
        return NULL;
    }
}
