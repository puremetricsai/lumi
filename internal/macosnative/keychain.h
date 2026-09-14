#ifndef LUMI_KEYCHAIN_H
#define LUMI_KEYCHAIN_H

#include <stdint.h>
#include <stddef.h>
#include <Security/Security.h>

int32_t LumiKeychainStore(const uint8_t *key, size_t length);
int32_t LumiKeychainLoad(uint8_t *out, size_t capacity, size_t *written);
int32_t LumiKeychainHas(void);
int32_t LumiKeychainDelete(void);
char *LumiKeychainMessage(int32_t status);

#endif
