#import <AppKit/AppKit.h>
#import <ApplicationServices/ApplicationServices.h>
#import <AVFoundation/AVFoundation.h>
#import <AudioToolbox/AudioToolbox.h>
#import <CoreGraphics/CoreGraphics.h>
#import <ImageIO/ImageIO.h>
#import <ScreenCaptureKit/ScreenCaptureKit.h>
#import <Vision/Vision.h>

#include <stdlib.h>
#include <stdbool.h>

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
    CGPoint center = CGPointMake(position.x + size.width / 2.0, position.y + size.height / 2.0);
    CGDirectDisplayID displays[32];
    uint32_t count = 0;
    if (CGGetOnlineDisplayList(32, displays, &count) != kCGErrorSuccess) return 0;
    for (uint32_t i = 0; i < count; i++) {
        if (CGRectContainsPoint(CGDisplayBounds(displays[i]), center)) return displays[i];
    }
    return 0;
}

char *lumi_accessibility_snapshot_json(char **error_message) {
    @autoreleasepool {
        NSRunningApplication *frontmost = NSWorkspace.sharedWorkspace.frontmostApplication;
        if (frontmost == nil) return LumiCopyUTF8(@"{\"app\":\"\",\"window\":\"\",\"text\":\"\",\"input_active\":false}");
        AXUIElementRef application = AXUIElementCreateApplication(frontmost.processIdentifier);
        AXUIElementSetMessagingTimeout(application, 1.0);
        CFTypeRef windowValue = NULL;
        AXError windowError = AXUIElementCopyAttributeValue(application, kAXFocusedWindowAttribute, &windowValue);
        if (windowError != kAXErrorSuccess || windowValue == NULL) {
            CFRelease(application);
            if (error_message != NULL) {
                NSString *message = [NSString stringWithFormat:@"read focused Accessibility window (AX error %d)", windowError];
                *error_message = LumiCopyUTF8(message);
            }
            return NULL;
        }
        AXUIElementRef window = (AXUIElementRef)windowValue;
        NSString *title = LumiAXString(window, kAXTitleAttribute);
        NSMutableOrderedSet<NSString *> *lines = [NSMutableOrderedSet orderedSet];
        NSUInteger visited = 0;
        LumiCollectAXText(window, lines, 0, &visited);
        NSString *text = [lines.array componentsJoinedByString:@"\n"];
        BOOL inputActive = CGEventSourceSecondsSinceLastEventType(kCGEventSourceStateCombinedSessionState,
                                                                  kCGAnyInputEventType) < 2.0;
        CGDirectDisplayID displayID = LumiAXDisplayID(window);
        NSDictionary *snapshot = @{@"app": frontmost.localizedName ?: @"",
                                   @"window": title ?: @"",
                                   @"text": text ?: @"",
                                   @"display_id": @(displayID),
                                   @"input_active": @(inputActive)};
        CFRelease(windowValue);
        CFRelease(application);
        NSError *jsonError = nil;
        NSString *json = LumiJSONString(snapshot, &jsonError);
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

char *lumi_permissions_json(char **error_message) {
    @autoreleasepool {
        NSDictionary *permissions = @{
            @"screen_recording": CGPreflightScreenCaptureAccess() ? @"granted" : @"denied_or_not_determined",
            @"accessibility": AXIsProcessTrusted() ? @"granted" : @"denied_or_not_determined",
            @"input_monitoring": CGPreflightListenEventAccess() ? @"granted" : @"denied_or_not_determined",
            @"microphone": LumiAuthorizationName([AVCaptureDevice authorizationStatusForMediaType:AVMediaTypeAudio]),
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
