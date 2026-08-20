# Changelog

## [0.6.0](https://github.com/puremetricsai/lumi/compare/v0.5.0...v0.6.0) (2026-08-20)


### Features

* **capture:** drain the live meters on a ticker instead of measuring a file ([91d1fb5](https://github.com/puremetricsai/lumi/commit/91d1fb5974bb6edee8c726e7215c5009cf9ef796))
* **cli:** add --json to mcp setup so the app can read registration status ([c562364](https://github.com/puremetricsai/lumi/commit/c562364b157a105c26c39dc0d6f0b41e472a74b1))
* **cli:** add the lumi app launcher and app-owned recorder registration ([a9ec45d](https://github.com/puremetricsai/lumi/commit/a9ec45d542ee7e29bb89a01092e31152e6a60677))
* **macos:** add compress to the Danger tab and measure it up front ([20e95c7](https://github.com/puremetricsai/lumi/commit/20e95c7da2cbbfb64b44f0434c0cd28cca9498cd))
* **macos:** add Lumi.app, a menu-bar supervisor for the lumi CLI ([1a17a26](https://github.com/puremetricsai/lumi/commit/1a17a268a8e4b797e0537be186e5f0345a2ed713))
* **macos:** add the five remaining Settings tabs ([f96de95](https://github.com/puremetricsai/lumi/commit/f96de95f7fc771d8030f59ed96391e9ed752bcc5))
* **macos:** add the Lumi.app menu bar shell ([a842b8b](https://github.com/puremetricsai/lumi/commit/a842b8b13a6938850b164bde4c359131aaa5e418))
* **macos:** draw a track with no signal as having none ([2dac48f](https://github.com/puremetricsai/lumi/commit/2dac48f9883e94c22e7b6a6d133f7cc2e4c680fb))
* **macos:** finish menu bar app development workflow ([d410051](https://github.com/puremetricsai/lumi/commit/d410051a4c315137a7ce043fb0399e4e082d1b0a))
* **macos:** move Quit from the window footer to the menu bar menu ([c3acfbf](https://github.com/puremetricsai/lumi/commit/c3acfbfde1efe3f065928384f6ae2fb25846f04e))
* **macosnative:** sum audio energy inside the capture callback ([a17cfb3](https://github.com/puremetricsai/lumi/commit/a17cfb37214f667faca489eea7dba06ad9f9ce97))
* name Homebrew as the way to install and update the app ([12dd200](https://github.com/puremetricsai/lumi/commit/12dd20075c805ba79211b687175d42c0bb0fb410))
* **wav:** report a level from energy a caller already summed ([ddfcdf5](https://github.com/puremetricsai/lumi/commit/ddfcdf5da4d90df8a2c361b00dd441f0617a28d7))


### Bug Fixes

* **compress:** skip audio too short for the FLAC encoder ([e21f85f](https://github.com/puremetricsai/lumi/commit/e21f85ff8c0081d8ca355802fd46630432150422))
* **compress:** skip audio too short for the FLAC encoder ([6fffeab](https://github.com/puremetricsai/lumi/commit/6fffeabdb47f8c5c72a3a80966274a3091aaeadb))
* **macos:** close the dead band under the window footer, and unrule the chrome ([9bfdecd](https://github.com/puremetricsai/lumi/commit/9bfdecdf6652b27e06e56dc9b0dad8bb1201d67b))
* **macos:** give every ungranted permission its own route to System Settings ([9a2d91d](https://github.com/puremetricsai/lumi/commit/9a2d91db22f6e81a4dc44a0a5ee263cdac7a7791))

## [0.5.0](https://github.com/puremetricsai/lumi/compare/v0.4.0...v0.5.0) (2026-08-14)


### Features

* **compress:** report progress so a long run is not mistaken for a hang ([402daf7](https://github.com/puremetricsai/lumi/commit/402daf7b1897220ef0320785d0fd998ea3630dfc))
* **compress:** report progress, and re-encode several files at once for a measured 3.8x ([5c1373b](https://github.com/puremetricsai/lumi/commit/5c1373b671368e05e48d95731f5caa26b41dc5de))


### Bug Fixes

* **ci:** rewrite the formula from the release archive, not the source tarball ([17bc58b](https://github.com/puremetricsai/lumi/commit/17bc58b066a26a3c08e8540349f17f37af3c5f59))
* **compress:** bound workers by the work, and say what --workers does not do ([970364d](https://github.com/puremetricsai/lumi/commit/970364df704718a417091c37e36a7142b4ff2413))
* **compress:** report attempted files so a rejecting pass still moves ([6fb8201](https://github.com/puremetricsai/lumi/commit/6fb82014910c1cdd76b46b9dda76f9a83f15ca3d))
* **homebrew:** install the released binary instead of building from source ([cbdaf67](https://github.com/puremetricsai/lumi/commit/cbdaf674a8900541330f9e0508dce4df840cee55))
* **homebrew:** install the released binary instead of building from source ([855f6bb](https://github.com/puremetricsai/lumi/commit/855f6bbfc936556e88e1477dadfaafa685bb5cc7))


### Performance Improvements

* **compress:** re-encode several files at once for a measured 3.8x ([336ecba](https://github.com/puremetricsai/lumi/commit/336ecba005254cc40d7079a13cbd8055f577eb9c))

## [0.4.0](https://github.com/puremetricsai/lumi/compare/v0.3.0...v0.4.0) (2026-08-13)


### Features

* add lumi compress ([22b9be5](https://github.com/puremetricsai/lumi/commit/22b9be5644b55c4f4bc49ebbc341622e1e7c8ef6))
* **cli:** add lumi compress ([2ec30bf](https://github.com/puremetricsai/lumi/commit/2ec30bf57c1089e63bc3483c62719a07177d4eb2))
* **compress:** add the media compression engine ([78d1b0e](https://github.com/puremetricsai/lumi/commit/78d1b0e45d1dbc4f1d48088a6c40f3f32dc6e30e))
* **macosnative:** add HEIC transcode, FLAC encode, and PCM16 decode bridges ([a8d0c59](https://github.com/puremetricsai/lumi/commit/a8d0c5940fe29b7d3622d2037a565eaf5c7c523e))
* **store:** add conditional UpdateMediaPath and Vacuum ([194ee94](https://github.com/puremetricsai/lumi/commit/194ee9423b9941f173ee60871fa9113d821f1094))


### Bug Fixes

* **compress:** close path-ownership and recovery safety holes from review ([0a770ff](https://github.com/puremetricsai/lumi/commit/0a770ffe827c874d7b12481891981edf5381e4c3))
* **compress:** close the defects two independent reviews found ([09251f0](https://github.com/puremetricsai/lumi/commit/09251f04ae66edf484fac1b54891f3f21b9e34d3))
* **compress:** close the destination axis over symlinks, and the ambiguity folding introduced ([e276eb1](https://github.com/puremetricsai/lumi/commit/e276eb1474aa9ea8b11cbb57c709b8da626033c3))
* **compress:** close two data-loss holes a third review found ([5ae302f](https://github.com/puremetricsai/lumi/commit/5ae302f9c45dc4710edce6f9426227e9fe95d021))

## [0.3.0](https://github.com/puremetricsai/lumi/compare/v0.2.0...v0.3.0) (2026-08-07)


### ⚠ BREAKING CHANGES

* `vocabulary.txt` is no longer read, and `lumi transcribe` no longer accepts `--vocabulary` or `--no-vocabulary`. Existing `vocabulary.txt` files are left on disk and ignored; they can be deleted. No transcript changes, because the terms never applied.

### Features

* remove custom vocabulary biasing ([3580bdb](https://github.com/puremetricsai/lumi/commit/3580bdb3bf8b0849cb606203ee01d2b537408f7f))
* remove custom vocabulary biasing ([dc810b5](https://github.com/puremetricsai/lumi/commit/dc810b5d556314505b9b775cd4c9f389f0d1f6d1))


### Bug Fixes

* close MCP reexec race and retain silent chunks ([b9d7f4b](https://github.com/puremetricsai/lumi/commit/b9d7f4b04fef0bac32578d9ffc209cabeeac7d01))

## [0.2.0](https://github.com/puremetricsai/lumi/compare/v0.1.0...v0.2.0) (2026-08-05)


### Features

* **mcp:** replace the server process on upgrade, and report both skews ([4458015](https://github.com/puremetricsai/lumi/commit/445801537952dbf6755121cb4b7341cb69771baf))
* **mcp:** replace the server process on upgrade, and report both skews ([a873503](https://github.com/puremetricsai/lumi/commit/a87350327f28e3991522452c723da9d14040bc2a))


### Bug Fixes

* **brew:** update macOS dependency syntax ([d1b2f08](https://github.com/puremetricsai/lumi/commit/d1b2f082a4af7bbbcbeb3748bb3c45cdd7a7b1c9))
* **brew:** update macOS dependency syntax ([b5701a5](https://github.com/puremetricsai/lumi/commit/b5701a565187dd88d94f1046df58297f287a79fb))

## 0.1.0 (2026-08-03)


### ⚠ BREAKING CHANGES

* **mcp:** search_events and get_event return media_file plus a response-level media_dir instead of media_path; source_app returns presence instead of pid, samples and observations; audio events no longer return stream_offset_ms.
* the `collapse_audio_tracks` parameter of the `search_events` MCP tool and the `lumi search --collapse-audio` flag are removed. `lumi search --json` is now unconditionally a bare event array. Agents sending the removed parameter get a validation tool error, not a protocol error; clients re-read the tool schema on connect.
* delete the cerebras, llama.cpp, and llm packages
* remove the configure and llama commands
* remove the lumi ask command and its retrieval layer

### Features

* **ask:** add local llama.cpp inference provider ([fd6bc1c](https://github.com/puremetricsai/lumi/commit/fd6bc1c03202db704b2d1d74c590f84b793494ac))
* **ask:** add local llama.cpp inference provider ([a8f0079](https://github.com/puremetricsai/lumi/commit/a8f0079117aab56c497d5da4175b54eb4d6ecb4b))
* **ask:** derive directional and "last night" time windows ([4705661](https://github.com/puremetricsai/lumi/commit/4705661b525827afa9b67a7eeea0f98609164c81))
* **ask:** derive the query time window from the question ([e9fe104](https://github.com/puremetricsai/lumi/commit/e9fe1043384f6c0b4572477ae023b039c408753b))
* **ask:** derive time window from the question and answer in local time ([beed101](https://github.com/puremetricsai/lumi/commit/beed101075ea3daec4d18f777e594b6a471dd454))
* **ask:** directional and "last night" time windows ([963bd85](https://github.com/puremetricsai/lumi/commit/963bd85d94e46c37d5f09a6c0557c6c85a0754be))
* **ask:** drop recording-modality words from FTS terms ([a3cc162](https://github.com/puremetricsai/lumi/commit/a3cc1625edacd2caabd596fc5117471f448e80c2))
* **ask:** render activity context in the user's local timezone ([b3b2acd](https://github.com/puremetricsai/lumi/commit/b3b2acdd4d5a4f1bf3fd36711351b5bd8664b8b5))
* **ask:** render the interpreted time window in human-readable form ([748a743](https://github.com/puremetricsai/lumi/commit/748a743aaac7f5101a1b4708d5cc036189332f11))
* **ask:** retrieve by relevance and bound the context window ([c2b2c11](https://github.com/puremetricsai/lumi/commit/c2b2c11e6aaf4a69773b4af90cb328864fccbce5))
* **ask:** silence the partial-match retrieval note ([e898bf6](https://github.com/puremetricsai/lumi/commit/e898bf6b3a89256de1f1918f3f9243951ccf759e))
* **ask:** steer answers to second person and local time ([7b91739](https://github.com/puremetricsai/lumi/commit/7b91739d2664cdff9eee337f82cdb3fb82528ce2))
* attribute audio to what produced it, and measure chunk timestamps ([251930c](https://github.com/puremetricsai/lumi/commit/251930c78794c4ddf24397e18e9f665d1c79167b))
* attribute captured audio by origin and read it as one transcript ([0fa47a8](https://github.com/puremetricsai/lumi/commit/0fa47a80e2d78d990d578c511d8fc516954961c5))
* **audio:** collapse duplicate mic/system audio events with provenance ([30866e4](https://github.com/puremetricsai/lumi/commit/30866e4ec6304b9f8a90160b086ecf719974f2ce))
* **audio:** collapse duplicate mic/system audio events with provenance ([c2dfcbd](https://github.com/puremetricsai/lumi/commit/c2dfcbd8bce276254d88bfaa1397f7a6c1e103eb))
* **brew:** install Lumi from a Homebrew tap ([c7b1d95](https://github.com/puremetricsai/lumi/commit/c7b1d95c4dfdeb5498c7fd5a0f04c1ddc052dcc5))
* **capture:** add native macOS activity capture ([8bd3f73](https://github.com/puremetricsai/lumi/commit/8bd3f73c5f04a483762002750d3f0c2cd7244dd7))
* **capture:** add native macOS activity capture ([baf6117](https://github.com/puremetricsai/lumi/commit/baf6117631a16570efdfdea22cf07e6caa69f3e7))
* **capture:** add NativeSpeech SpeechTranscriber backed by SpeechAnalyzer ([c3dc2e1](https://github.com/puremetricsai/lumi/commit/c3dc2e1b1128f915887bb58bb06eeb667eb8f997))
* **capture:** attribute audio chunks to an app and its audio output ([1a25096](https://github.com/puremetricsai/lumi/commit/1a250966322879059a9e7276aa9e60f25241c624))
* **capture:** attribute audio to what produced it, sampled across the chunk ([e98429d](https://github.com/puremetricsai/lumi/commit/e98429dd7a41e9c565ea998e3ec7525184dad958))
* **capture:** attribute each audio chunk after both tracks are indexed ([e12d086](https://github.com/puremetricsai/lumi/commit/e12d0862a5e97b3c37d13bb01d4248fd8dffa0d7))
* **capture:** bias transcription with the user's vocabulary file ([e04b539](https://github.com/puremetricsai/lumi/commit/e04b539acf288992794a3783597f1c8975f057b3))
* **capture:** full-display Vision OCR as primary screen-text source ([8e4686a](https://github.com/puremetricsai/lumi/commit/8e4686a78df0004bd00ac7eccfc5a0f82bfbcc64))
* **capture:** keep the audio tap open across chunk boundaries ([f6a9281](https://github.com/puremetricsai/lumi/commit/f6a9281f2a0f8d5e62e21b95e4d7ca9944d4e835))
* **capture:** make full-display Vision OCR the primary screen-text source ([a3b6b3b](https://github.com/puremetricsai/lumi/commit/a3b6b3baef92089503c8c89a6a7580ce0da503aa))
* **capture:** record app_source provenance on screen events ([ca45540](https://github.com/puremetricsai/lumi/commit/ca455409d1b49ba8d885c0ca2acf4e55410f240c))
* **cli:** add `lumi mcp setup` to register the server with MCP clients ([f3b58ba](https://github.com/puremetricsai/lumi/commit/f3b58ba6139c7f14ade211571815e3a38fac16c9))
* **cli:** add `lumi transcript` and `lumi transcript backfill` ([e097a9d](https://github.com/puremetricsai/lumi/commit/e097a9d7ce3e83b6f316fbfde0929d8b3e028b20))
* **cli:** add lumi mcp setup to configure MCP clients ([5e3f464](https://github.com/puremetricsai/lumi/commit/5e3f46409a2307c3a28a1e35b6fcea61b94462fb))
* **cli:** add lumi transcribe for replaying a WAV with a vocabulary ([1cb36de](https://github.com/puremetricsai/lumi/commit/1cb36de838f830a37f69e31269d054d28760bac4))
* **cli:** add prune --all with confirmation prompt ([8966331](https://github.com/puremetricsai/lumi/commit/896633132b15760422951a767965842e206aa5e8))
* **cli:** add prune command with age and size retention ([bb2f5de](https://github.com/puremetricsai/lumi/commit/bb2f5decca92a148fb9b6d9f022369954c9a2cf7))
* **cli:** add the lumi mcp stdio server command ([cf16703](https://github.com/puremetricsai/lumi/commit/cf16703bd1a1d808a9dea7d959ff7e60aac4371c))
* **cli:** allow the version string to be set at link time ([a8cefde](https://github.com/puremetricsai/lumi/commit/a8cefde4dbf4047a0dd2329afd7c40151aafa7dc))
* **cli:** configure Codex CLI from `lumi mcp setup` ([93c81c3](https://github.com/puremetricsai/lumi/commit/93c81c33f004731cf4d4c95b061d473a6acee661))
* **cli:** report confidence removals on both transcript output paths ([55d2631](https://github.com/puremetricsai/lumi/commit/55d26313c8081af605f822e53b0ae4db5265c36f))
* **cli:** report speech capability in doctor and permissions ([cbb4576](https://github.com/puremetricsai/lumi/commit/cbb4576a9fcc423d5d4861e3704cee61b003391f))
* **cli:** report the frontmost sources in native-smoke ([7a5a09e](https://github.com/puremetricsai/lumi/commit/7a5a09e911700bd0a5f166563c1d0c8989039745))
* **cli:** report vocabulary state in lumi doctor ([27f1677](https://github.com/puremetricsai/lumi/commit/27f16770a505c9ad0be5c1dde329d2115c8c7508))
* **cli:** transcribe with SpeechAnalyzer and remove whisper from record ([d9732c8](https://github.com/puremetricsai/lumi/commit/d9732c8293a7e1da23c945965aa70d948e3d8f5b))
* **cli:** wire audio output attribution and report it in native-smoke ([88d6ab7](https://github.com/puremetricsai/lumi/commit/88d6ab728a62acecf4069259fdc12157a850a9d9))
* **cli:** wire the marker scan and restate the microphone caveat ([2511828](https://github.com/puremetricsai/lumi/commit/25118283a37e827bc328d5868e30a1c3759b1150))
* **config:** add the vocabulary file path under the data root ([eaa5372](https://github.com/puremetricsai/lumi/commit/eaa53721895338190e8fb1fa916a7dff215d9727))
* **config:** persist Cerebras credentials via lumi configure ([0e82be5](https://github.com/puremetricsai/lumi/commit/0e82be5ed0838fd8f69be859be08c824e999454a))
* **config:** persist Cerebras credentials via lumi configure ([256844c](https://github.com/puremetricsai/lumi/commit/256844ce1260f2a837da6d1ceeb69e77e34c10e4))
* **configure:** offer cached llama.cpp models by number ([9bd39a9](https://github.com/puremetricsai/lumi/commit/9bd39a99163d15349210951638c9fc22419ee0b7))
* delete the cerebras, llama.cpp, and llm packages ([61dd3c4](https://github.com/puremetricsai/lumi/commit/61dd3c4c1cf81b1cbb682e0bbfd8b7967622dfe2))
* **doctor:** report observed attribution health, not just TCC status ([ffe8d33](https://github.com/puremetricsai/lumi/commit/ffe8d335b2d6ee8ec76ea47ee7d804c1f3896024))
* **llamacpp:** restart a warm llama-server when the model changes ([945ae8c](https://github.com/puremetricsai/lumi/commit/945ae8c105fc182885bc3bf854a88b9213bb35c7))
* **llamacpp:** restart a warm llama-server when the model changes ([508807c](https://github.com/puremetricsai/lumi/commit/508807c88bf72615e0cb0f841455a32bd26e08c4))
* **macosnative:** measure chunk timestamps and find audio-marker windows ([28b53ae](https://github.com/puremetricsai/lumi/commit/28b53ae97e63aa8a527d8acb6e6e17312c167e2f))
* **macosnative:** read the processes holding an audio output stream ([4f8ebf5](https://github.com/puremetricsai/lumi/commit/4f8ebf5e8620bad14f5898c9cf28d4f91e6667b1))
* **macosnative:** report and request Speech Recognition authorization ([3ddda73](https://github.com/puremetricsai/lumi/commit/3ddda73e152d384cbc8fdb5af568432ae9e059e7))
* **macosnative:** transcribe audio with on-device SpeechAnalyzer ([5bce293](https://github.com/puremetricsai/lumi/commit/5bce29304d7c82cc8d2562086450419c7a8b4ff1))
* **mcp:** add parameter parsing and rune-safe truncation ([0d5c9e5](https://github.com/puremetricsai/lumi/commit/0d5c9e55773de556380c699329d6e17d7ef4c20d))
* **mcp:** add the get_event and list_apps tool handlers ([341e0c1](https://github.com/puremetricsai/lumi/commit/341e0c1a51cd457c02cebedd7e34b795b3824ee1))
* **mcp:** add the get_transcript tool ([f191800](https://github.com/puremetricsai/lumi/commit/f191800a37f8e94ab025d4d4243f99b9a9a2c19a))
* **mcp:** add the search_events tool handler ([f8cfd80](https://github.com/puremetricsai/lumi/commit/f8cfd801958891cd68317b9c1de0814a58e5e262))
* **mcp:** keep an audio row's device, source, and foreground app apart ([d904a89](https://github.com/puremetricsai/lumi/commit/d904a895ed368d4a54ab38333de0e5f2c88399e6))
* **mcp:** report the screen/audio split behind an app's count in list_apps ([9553d4b](https://github.com/puremetricsai/lumi/commit/9553d4b52f3c042e1cbbc1eb9369c5eaf81690b9))
* **mcp:** say what min_confidence removed from a transcript that is not empty ([f433740](https://github.com/puremetricsai/lumi/commit/f43374060cb2c9c8bb2d9fc32e982b87a3d2ac4b))
* **mcp:** serve search_events, get_event and list_apps over stdio ([fac3ec4](https://github.com/puremetricsai/lumi/commit/fac3ec4747e51143d6c90511b2a4e756b238d5a8))
* **mcp:** serve the activity index to AI agents over MCP ([14c3c08](https://github.com/puremetricsai/lumi/commit/14c3c08f3b389e3df512b033119805633b828036))
* **mcpsetup:** add a Codex CLI target ([a38253c](https://github.com/puremetricsai/lumi/commit/a38253c89589783c60928a54697c10ece0e1e94e))
* **mcpsetup:** register lumi mcp with installed MCP clients ([d15eee0](https://github.com/puremetricsai/lumi/commit/d15eee0338715d60262b16ce8f8132d63dfd35f9))
* **mcpsetup:** register lumi with Codex CLI from `lumi mcp setup` ([7f7e2db](https://github.com/puremetricsai/lumi/commit/7f7e2db487cb01012be0226d3fd634d22095c118))
* **platform:** require macOS 26 for SpeechAnalyzer ([91952fa](https://github.com/puremetricsai/lumi/commit/91952fa148f42af44f8b5c10fb02009b2886ab5c))
* **prune:** add `--all` to wipe all data with a confirmation guard ([00622ae](https://github.com/puremetricsai/lumi/commit/00622ae83881ed041bc2727835c69b0ba60623a2))
* **record:** announce graceful stop in record stop ([44665ba](https://github.com/puremetricsai/lumi/commit/44665ba4d651b5e4899d513670f60696c0415d8f))
* **record:** announce graceful stop in record stop ([28ffc6a](https://github.com/puremetricsai/lumi/commit/28ffc6a6a15e448f34fb788da279c0e3217e9dbc))
* **record:** run recording as a background task with start/status/stop ([95a892e](https://github.com/puremetricsai/lumi/commit/95a892e908878e89f56b6e32b94ba760c8f3f8ce))
* **record:** run recording as a background task with start/status/stop ([26cbb1c](https://github.com/puremetricsai/lumi/commit/26cbb1c279c1c44894f7f91fd5b53b86ce82ae9e))
* release automation and Homebrew install ([87520e8](https://github.com/puremetricsai/lumi/commit/87520e85137d7070e1d27878772a15444565aad8))
* **release:** automate releases with Release Please ([88dffc0](https://github.com/puremetricsai/lumi/commit/88dffc0ed88f69a0d75099ee59ebc180a1c4d738))
* remove the configure and llama commands ([88047d3](https://github.com/puremetricsai/lumi/commit/88047d302a1e3d914376113645f4b91d547e77e5))
* remove the lumi ask command and its retrieval layer ([78d8787](https://github.com/puremetricsai/lumi/commit/78d87879a6d44dd59eb1e0adb0e34efa9e6bd783))
* **retention:** add All wipe-everything prune policy ([b91deb4](https://github.com/puremetricsai/lumi/commit/b91deb4e46b30b283720a2173c1bdcb2fb48f099))
* **search:** filter by app and window title ([098494d](https://github.com/puremetricsai/lumi/commit/098494d8562ebf8c7f27b4848a736faf4cbc9af4))
* **speech:** apply contextual vocabulary terms via AnalysisContext ([28c7e50](https://github.com/puremetricsai/lumi/commit/28c7e5028d462c0df2cffc823e2e9c66ac8d99ee))
* **speech:** custom vocabulary to bias on-device transcription ([fb65357](https://github.com/puremetricsai/lumi/commit/fb653575d319e28d18468adab5754c9913f1591e))
* **speech:** pre-download assets and detect install state reliably ([ccf9ad9](https://github.com/puremetricsai/lumi/commit/ccf9ad948a47e5c6091c5651f31dd4c5613a068f))
* **speech:** return word timings and a real audio start anchor ([5a09689](https://github.com/puremetricsai/lumi/commit/5a096897a86e56a2dbce49900d8997afe21f2190))
* **store:** add audio source columns and one canonical captured_at layout ([d293b4e](https://github.com/puremetricsai/lumi/commit/d293b4ec4fd0bba898f4f611e849f8404e023d33))
* **store:** add EventByID with an ErrEventNotFound sentinel ([67c6009](https://github.com/puremetricsai/lumi/commit/67c6009d30f8f041b1120893f0c57ee616c5207c))
* **store:** add Expired and DeleteByIDs for retention ([8908125](https://github.com/puremetricsai/lumi/commit/89081259f212f8c31553340998a74e8cc8178a2c))
* **store:** add ListAttribution for app and window inventories ([0be66ad](https://github.com/puremetricsai/lumi/commit/0be66ad083c78d71fd8ee33f5ace69f0a6de4118))
* **store:** add versioned migrations tracked by user_version ([d11f7c3](https://github.com/puremetricsai/lumi/commit/d11f7c3a27b0f46fb5c94c2b49e351de33b0748b))
* **store:** report which turns min_confidence removed, by origin ([bfc0167](https://github.com/puremetricsai/lumi/commit/bfc0167656403fd96f6c2d7eddeec4ce6f5d0867))
* **store:** store attributed audio segments and serve transcripts ([f2ed978](https://github.com/puremetricsai/lumi/commit/f2ed9780dd756672292cf967024642be9c6f67bb))
* Tier 1 — versioned migrations, app/window filters, retention ([bdf95e8](https://github.com/puremetricsai/lumi/commit/bdf95e80fca1f2351375dcd62771507e17fe7418))
* **transcript:** attribute captured audio by origin and assemble turns ([3a6d697](https://github.com/puremetricsai/lumi/commit/3a6d6973e07007f19a19b99882d6c61825626d01))
* **transcript:** report what min_confidence removed instead of deleting one side of a conversation silently ([0b86bf0](https://github.com/puremetricsai/lumi/commit/0b86bf0ac56fa743d4817d453d01c0786094aed0))
* **vocabulary:** cache successful reads and always retry failures ([ac4df10](https://github.com/puremetricsai/lumi/commit/ac4df1004bd2ec839072587cfa21e77b21f6c61e))
* **vocabulary:** parse the term list file and cap it at MaxTerms ([f31a7a4](https://github.com/puremetricsai/lumi/commit/f31a7a4551dbc73a7027fed15f5b09d4eb05ce6e))
* **wav:** read Lumi's own WAV layout and measure its energy ([20669dd](https://github.com/puremetricsai/lumi/commit/20669dd1fa62ad048a918782bf89549086c5a6af))


### Bug Fixes

* **ask:** answer recording questions from the audio corpus ([790e43b](https://github.com/puremetricsai/lumi/commit/790e43b8e7058fa8573751efc90287f4f4f919fd))
* **ask:** answer recording questions from the audio corpus ([4595083](https://github.com/puremetricsai/lumi/commit/4595083d1ada82fe54bcf34fb4d7ad8ee90e822a))
* **ask:** improve broad activity summaries ([2cb99f7](https://github.com/puremetricsai/lumi/commit/2cb99f7949d362ebf58088e521864c75062aa2ea))
* **ask:** improve broad activity summaries ([342e2e6](https://github.com/puremetricsai/lumi/commit/342e2e63cc08020c972a05fb846e589c169ec223))
* **ask:** make the inference client reasoning-aware ([145f19a](https://github.com/puremetricsai/lumi/commit/145f19a8183a074e0b2964163f2017d852a7890e))
* **ask:** make the inference client reasoning-aware ([1798395](https://github.com/puremetricsai/lumi/commit/1798395d8fd6f46e2543a3d86990dc6c39c4a36a))
* **ask:** resolve "day before yesterday" to the right day ([9e1bbe4](https://github.com/puremetricsai/lumi/commit/9e1bbe4e873d4e655696bb5f82be11556fee794c))
* **ask:** resolve "day before yesterday" to the right day ([2e7a867](https://github.com/puremetricsai/lumi/commit/2e7a867cc3055a5e30b22909bdf6cabafd7480d9))
* **attribution:** resolve the frontmost pid from a live source ([97d1a7f](https://github.com/puremetricsai/lumi/commit/97d1a7f033793c81565d2496a7fedb675f237e52))
* **attribution:** resolve the frontmost pid from a live source ([d4c24e6](https://github.com/puremetricsai/lumi/commit/d4c24e68f176a0102043e6fbc5bc10f0ffa43a43))
* **attribution:** validate frontmost candidates against activation ([017cc2d](https://github.com/puremetricsai/lumi/commit/017cc2d6efc14eb071ef2d2d6a42e8352e2f491d))
* **capture:** keep app attribution when Accessibility reads fail ([122077e](https://github.com/puremetricsai/lumi/commit/122077e632906c5e4851f6ce839547dd9af54663))
* **capture:** keep app attribution when Accessibility reads fail ([ab9d8e8](https://github.com/puremetricsai/lumi/commit/ab9d8e897ab01bdd2dc9719787addc847300991e))
* **capture:** never record a failed transcription as silence ([4d40375](https://github.com/puremetricsai/lumi/commit/4d40375abf7122eca7777852a25424cc04f558f9))
* **cli:** default search --limit from the store's constant ([5c26a4a](https://github.com/puremetricsai/lumi/commit/5c26a4af17c6587367a5708af0ce24c63161faea))
* **cli:** let a re-transcription supply timings but never text ([d49fb46](https://github.com/puremetricsai/lumi/commit/d49fb46e51def6fcf17eecbc474a6384b022e623))
* **macosnative:** rename SpeechStatus type to SpeechCapability ([9864040](https://github.com/puremetricsai/lumi/commit/986404079ab2d5367457de549cd940c7be9b8c5c))
* **mcp,cli:** render resume_from in the local zone like every other time ([fb7920d](https://github.com/puremetricsai/lumi/commit/fb7920d18247a1db1c16a3a191e7780b66b123b1))
* **mcp:** add MCP SDK as direct dependency in go.mod ([c278ef3](https://github.com/puremetricsai/lumi/commit/c278ef3436329e1b5b9e8723fa217b713671820f))
* **mcp:** always serialize truncated, and correct docs this branch outdated ([e6c8659](https://github.com/puremetricsai/lumi/commit/e6c865958300077e232c3a5074b87e372bb485f2))
* **mcp:** reject unsearchable queries and surface capped/empty result notices ([a39cc98](https://github.com/puremetricsai/lumi/commit/a39cc9886d5c084459658dc6a269907705835dc8))
* **mcp:** send each payload as text again for clients that read only Content ([377646b](https://github.com/puremetricsai/lumi/commit/377646b43acc23d0551a6f35faa7db20b710d0a3))
* **mcpsetup:** close four destructive paths in mcp setup ([3dcfa11](https://github.com/puremetricsai/lumi/commit/3dcfa110b7bf4b9b193b68b9ae8714de95e32ce4))
* **mcp:** stop the stdio server from dropping in-flight responses on stdin EOF ([803bb92](https://github.com/puremetricsai/lumi/commit/803bb92f5059f90b086eb06eb0826021279da2ea))
* **permissions:** distinguish denied Input Monitoring from never asked ([63a0646](https://github.com/puremetricsai/lumi/commit/63a0646575454f30b238668fd206416c1de0741e))
* **record:** stabilize background recorder lifecycle ([f118502](https://github.com/puremetricsai/lumi/commit/f11850241d5b715355d82787744f5e51668fb051))
* **record:** stabilize background recorder lifecycle ([0179f91](https://github.com/puremetricsai/lumi/commit/0179f91b246dc1d3e9a5aaa39492bfd5372962db))
* report attribution degradation without overstating it ([c2285f8](https://github.com/puremetricsai/lumi/commit/c2285f85f6d193984bf311e37004e039872f5281))
* **retention:** correct dry-run accounting when age and size prune combine ([2eaf156](https://github.com/puremetricsai/lumi/commit/2eaf156c3a16de637db6fe3b2101b870a280b00e))
* **retention:** make prune --all a complete, irreversible wipe ([51f3aa2](https://github.com/puremetricsai/lumi/commit/51f3aa29bbbc21a218e1749d1b75c8088f65ad62))
* **store:** add opt-in RequireText search filter ([a0265cb](https://github.com/puremetricsai/lumi/commit/a0265cb7728569ee9237acf4a4b5ee9874865a60))
* **store:** batch DeleteByIDs to stay under SQLite's variable limit ([ef6136a](https://github.com/puremetricsai/lumi/commit/ef6136a40d218b4189b30e4a9699305296a92deb))
* **store:** measure coverage and paging from the turns actually returned ([43ec17c](https://github.com/puremetricsai/lumi/commit/43ec17c85f59c08420f53a45ad6d1e4bba85d21f))
* **taskfile:** invoke record start in the smoke and record targets ([3d6eb2b](https://github.com/puremetricsai/lumi/commit/3d6eb2bca5ba3eb7d6a9a68827f67aea7eafce99))
* **vocabulary:** report dropped terms, reject empty paths, isolate setContext failures ([3cdd54c](https://github.com/puremetricsai/lumi/commit/3cdd54c051d8f623b1ef048e519d827cfccdbb11))
* **vocabulary:** return terms the caller cannot mutate into the cache ([d991439](https://github.com/puremetricsai/lumi/commit/d991439f2b1013f470e9f67fdcb28854799db0cd))
* **vocabulary:** split lines without bufio's 64KB token limit ([05a4a23](https://github.com/puremetricsai/lumi/commit/05a4a23d834ed01cde33690b04dab5b6a8210991))


### Performance Improvements

* **capture:** stop re-indexing byte-identical screenshots ([6fa07ff](https://github.com/puremetricsai/lumi/commit/6fa07ff3a1992cf4a8866422de545307912dd512))
* **capture:** stop re-indexing byte-identical screenshots ([64484cf](https://github.com/puremetricsai/lumi/commit/64484cf0391b475908e6f0038577861ea752bf7d))
* **wav:** reduce a file to its envelope without materializing the stream ([c357092](https://github.com/puremetricsai/lumi/commit/c3570922da5a7f869f88284d2e1bfa8168a1eab6))


### Code Refactoring

* **mcp:** send each payload once and drop what restates it ([4566c00](https://github.com/puremetricsai/lumi/commit/4566c00003b2f343014560cd4abe371b2babe97e))
* remove audio collapse from search ([5c70bf1](https://github.com/puremetricsai/lumi/commit/5c70bf16e2e993251e120c015084e2d890389d09))
