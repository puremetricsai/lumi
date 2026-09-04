#import <Foundation/Foundation.h>
#import <Security/Security.h>
#import <string.h>

#import "keychain.h"

// The service and account naming Lumi's key.
//
// The account is a fixed string rather than the data directory's path. Keying it
// on the path would mean Storage settings' "Choose…" relocation moved the store
// to a location whose key does not exist — an encrypted index nothing can read,
// produced by a button that says nothing about encryption. One key per user
// covers every data directory this user has, which is the right trade.
static NSString *const kLumiKeychainService = @"com.puremetricsai.lumi";
static NSString *const kLumiKeychainAccount = @"encryption-key";

static NSMutableDictionary *LumiKeychainQuery(void) {
    return [@{
        (id)kSecClass: (id)kSecClassGenericPassword,
        (id)kSecAttrService: kLumiKeychainService,
        (id)kSecAttrAccount: kLumiKeychainAccount,
    } mutableCopy];
}

int32_t LumiKeychainStore(const uint8_t *key, size_t length) {
    @autoreleasepool {
        if (key == NULL || length == 0) {
            return errSecParam;
        }

        // Replace rather than update: an existing item carries an ACL trusting
        // whatever binary wrote it, and after a rebuild that is a code identity
        // this process no longer has. Adding fresh re-establishes the ACL.
        SecItemDelete((__bridge CFDictionaryRef)LumiKeychainQuery());

        SecTrustedApplicationRef self = NULL;
        OSStatus status = SecTrustedApplicationCreateFromPath(NULL, &self);
        if (status != errSecSuccess) {
            return (int32_t)status;
        }

        SecAccessRef access = NULL;
        status = SecAccessCreate(CFSTR("Lumi captured history"),
                                 (__bridge CFArrayRef)@[(__bridge id)self],
                                 &access);
        CFRelease(self);
        if (status != errSecSuccess) {
            return (int32_t)status;
        }

        NSMutableDictionary *item = LumiKeychainQuery();
        item[(id)kSecValueData] = [NSData dataWithBytes:key length:length];
        item[(id)kSecAttrAccess] = (__bridge id)access;
        item[(id)kSecAttrLabel] = @"Lumi captured history";
        item[(id)kSecAttrDescription] = @"Encrypts Lumi's screenshots, audio, and search index";

        status = SecItemAdd((__bridge CFDictionaryRef)item, NULL);
        CFRelease(access);
        return (int32_t)status;
    }
}

int32_t LumiKeychainLoad(uint8_t *out, size_t capacity, size_t *written) {
    @autoreleasepool {
        if (out == NULL || written == NULL) {
            return errSecParam;
        }
        *written = 0;

        NSMutableDictionary *query = LumiKeychainQuery();
        query[(id)kSecReturnData] = @YES;
        query[(id)kSecMatchLimit] = (id)kSecMatchLimitOne;

        CFTypeRef result = NULL;
        OSStatus status = SecItemCopyMatching((__bridge CFDictionaryRef)query, &result);
        if (status != errSecSuccess) {
            return (int32_t)status;
        }

        NSData *data = (__bridge_transfer NSData *)result;
        if (data.length > capacity) {
            return errSecParam;
        }
        memcpy(out, data.bytes, data.length);
        *written = data.length;
        return errSecSuccess;
    }
}

int32_t LumiKeychainHas(void) {
    @autoreleasepool {
        // kSecReturnAttributes, never kSecReturnData. Reading the data is gated
        // by the ACL and prompts any process that is not this binary; reading
        // the attributes is ungated. Every caller that only needs to know
        // whether encryption is on comes through here, so refusing a command
        // costs nobody a Keychain dialog.
        NSMutableDictionary *query = LumiKeychainQuery();
        query[(id)kSecReturnAttributes] = @YES;
        query[(id)kSecMatchLimit] = (id)kSecMatchLimitOne;

        CFTypeRef result = NULL;
        OSStatus status = SecItemCopyMatching((__bridge CFDictionaryRef)query, &result);
        if (result != NULL) {
            CFRelease(result);
        }
        return (int32_t)status;
    }
}

int32_t LumiKeychainDelete(void) {
    @autoreleasepool {
        return (int32_t)SecItemDelete((__bridge CFDictionaryRef)LumiKeychainQuery());
    }
}

char *LumiKeychainMessage(int32_t status) {
    @autoreleasepool {
        CFStringRef message = SecCopyErrorMessageString((OSStatus)status, NULL);
        if (message == NULL) {
            return NULL;
        }
        const char *utf8 = [(__bridge NSString *)message UTF8String];
        char *copy = utf8 != NULL ? strdup(utf8) : NULL;
        CFRelease(message);
        return copy;
    }
}
