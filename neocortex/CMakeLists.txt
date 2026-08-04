cmake_minimum_required(VERSION 3.28.3)

project(neocortex VERSION 0.1.0 LANGUAGES C CXX)

include(CTest)

set(CMAKE_CXX_STANDARD 23)
set(CMAKE_CXX_STANDARD_REQUIRED ON)
set(CMAKE_CXX_EXTENSIONS OFF)

set(NEOCORTEX_SANITIZER "none" CACHE STRING "none, address-undefined, or thread")
set_property(CACHE NEOCORTEX_SANITIZER PROPERTY STRINGS none address-undefined thread)
option(NEOCORTEX_FUZZING "Build libFuzzer targets" OFF)
option(NEOCORTEX_CLANG_TIDY "Run clang-tidy while compiling" OFF)
option(NEOCORTEX_STATIC "Link a reproducible static runtime" OFF)

include(toolchain/libsodium.cmake)

if(NOT CMAKE_CXX_COMPILER_ID STREQUAL "Clang")
  message(FATAL_ERROR "Neocortex requires the pinned Clang toolchain")
endif()

if(NOT CMAKE_CXX_COMPILER_VERSION VERSION_EQUAL "18.1.3")
  message(FATAL_ERROR "Neocortex requires Clang 18.1.3, found ${CMAKE_CXX_COMPILER_VERSION}")
endif()

if(NEOCORTEX_CLANG_TIDY)
  find_program(NEOCORTEX_CLANG_TIDY_BIN NAMES clang-tidy-18 REQUIRED)
endif()

set(CRC32C_BUILD_TESTS OFF CACHE BOOL "" FORCE)
set(CRC32C_BUILD_BENCHMARKS OFF CACHE BOOL "" FORCE)
set(CRC32C_USE_GLOG OFF CACHE BOOL "" FORCE)
set(CRC32C_INSTALL OFF CACHE BOOL "" FORCE)
add_subdirectory(third_party/hash/crc32c EXCLUDE_FROM_ALL)
add_subdirectory(third_party/blake3/c EXCLUDE_FROM_ALL)

function(neocortex_target target)
  if(NEOCORTEX_CLANG_TIDY)
    set_property(TARGET ${target} PROPERTY CXX_CLANG_TIDY
      "${NEOCORTEX_CLANG_TIDY_BIN};--config-file=${CMAKE_CURRENT_SOURCE_DIR}/toolchain/.clang-tidy"
    )
  endif()
  target_compile_options(${target} PRIVATE
    -Wall -Wextra -Wpedantic -Wconversion -Wsign-conversion -Wshadow -Werror
    -fno-exceptions -fno-rtti
    "-ffile-prefix-map=${CMAKE_SOURCE_DIR}=."
    "-fdebug-prefix-map=${CMAKE_SOURCE_DIR}=."
  )
  if(CMAKE_BUILD_TYPE STREQUAL "Release")
    target_compile_definitions(${target} PRIVATE _LIBCPP_HARDENING_MODE=_LIBCPP_HARDENING_MODE_FAST)
  else()
    target_compile_definitions(${target} PRIVATE _LIBCPP_HARDENING_MODE=_LIBCPP_HARDENING_MODE_EXTENSIVE)
  endif()
  if(NEOCORTEX_SANITIZER STREQUAL "address-undefined")
    target_compile_options(${target} PRIVATE -fsanitize=address,undefined -fno-omit-frame-pointer)
    target_link_options(${target} PRIVATE -fsanitize=address,undefined -fno-omit-frame-pointer)
  elseif(NEOCORTEX_SANITIZER STREQUAL "thread")
    target_compile_options(${target} PRIVATE -fsanitize=thread -fno-omit-frame-pointer)
    target_link_options(${target} PRIVATE -fsanitize=thread -fno-omit-frame-pointer)
  elseif(NOT NEOCORTEX_SANITIZER STREQUAL "none")
    message(FATAL_ERROR "Unknown sanitizer: ${NEOCORTEX_SANITIZER}")
  endif()
  if(NEOCORTEX_STATIC)
    target_link_options(${target} PRIVATE -static -Wl,--build-id=none)
  endif()
endfunction()

add_library(neocortex_log STATIC
  src/log/frame.cc
  src/log/segment_log.cc
)
target_include_directories(neocortex_log PUBLIC src)
target_include_directories(neocortex_log SYSTEM PRIVATE third_party/liburing/src/include)
target_link_libraries(neocortex_log PRIVATE crc32c)
neocortex_target(neocortex_log)

add_executable(neocortex_frame_test test/frame_test.cc)
target_link_libraries(neocortex_frame_test PRIVATE neocortex_log)
neocortex_target(neocortex_frame_test)
add_test(NAME frame_codec COMMAND neocortex_frame_test)

add_executable(neocortex_segment_log_test test/segment_log_test.cc)
target_link_libraries(neocortex_segment_log_test PRIVATE neocortex_log)
neocortex_target(neocortex_segment_log_test)
add_test(NAME segment_log COMMAND neocortex_segment_log_test)

add_library(neocortex_schema STATIC src/schema/events.cc)
target_include_directories(neocortex_schema PUBLIC src)
target_include_directories(neocortex_schema SYSTEM PUBLIC
  third_party/flatbuffers/include
)
target_link_libraries(neocortex_schema PUBLIC neocortex_log)
neocortex_target(neocortex_schema)

add_executable(neocortex_schema_test test/schema_test.cc)
target_link_libraries(neocortex_schema_test PRIVATE neocortex_schema neocortex_mmr)
neocortex_target(neocortex_schema_test)
add_test(NAME event_schema COMMAND neocortex_schema_test)

add_library(neocortex_gate STATIC src/gate/belief_gate.cc)
target_include_directories(neocortex_gate PUBLIC src)
target_include_directories(neocortex_gate SYSTEM PUBLIC
  third_party/flatbuffers/include
)
target_link_libraries(neocortex_gate PUBLIC neocortex_schema)
neocortex_target(neocortex_gate)

add_library(neocortex_mmr STATIC
  src/mmr/mmr.cc
  src/mmr/store.cc
)
target_include_directories(neocortex_mmr PUBLIC src)
target_link_libraries(neocortex_mmr
  PRIVATE
    blake3
    crc32c
    neocortex_sodium
)
neocortex_target(neocortex_mmr)

add_library(neocortex_seal STATIC src/seal/sealing.cc)
target_include_directories(neocortex_seal PUBLIC src)
target_link_libraries(neocortex_seal PUBLIC neocortex_mmr neocortex_sodium)
neocortex_target(neocortex_seal)

add_executable(cortex-verify cmd/cortex-verify/main.cc)
target_link_libraries(cortex-verify PRIVATE neocortex_mmr)
neocortex_target(cortex-verify)

add_executable(neocortex_mmr_test test/mmr_test.cc)
target_link_libraries(neocortex_mmr_test PRIVATE neocortex_mmr)
neocortex_target(neocortex_mmr_test)
add_test(NAME mmr_checkpoint COMMAND neocortex_mmr_test $<TARGET_FILE:cortex-verify>)

add_executable(neocortex_seal_test test/crypto_test.cc)
target_link_libraries(neocortex_seal_test PRIVATE neocortex_seal neocortex_sim)
neocortex_target(neocortex_seal_test)
add_test(NAME record_sealing COMMAND neocortex_seal_test)

add_library(neocortex_core STATIC
  src/core/apply.cc
  src/core/env_posix.cc
  src/log/storage_adapter.cc
)
target_include_directories(neocortex_core PUBLIC src)
target_link_libraries(neocortex_core PUBLIC neocortex_log neocortex_mmr neocortex_seal
                                              neocortex_schema)
neocortex_target(neocortex_core)

add_library(neocortex_sim STATIC src/sim/sim_env.cc)
target_include_directories(neocortex_sim PUBLIC src)
target_link_libraries(neocortex_sim PUBLIC neocortex_core)
neocortex_target(neocortex_sim)

add_executable(neocortex_core_sim_test test/core_sim_test.cc)
target_link_libraries(neocortex_core_sim_test PRIVATE neocortex_sim)
neocortex_target(neocortex_core_sim_test)
add_test(NAME deterministic_core_sim COMMAND neocortex_core_sim_test)

add_library(neocortex_lmdb STATIC
  third_party/lmdb/libraries/liblmdb/mdb.c
  third_party/lmdb/libraries/liblmdb/midl.c
)
find_package(Threads REQUIRED)
target_include_directories(neocortex_lmdb SYSTEM PUBLIC
  third_party/lmdb/libraries/liblmdb
)
target_link_libraries(neocortex_lmdb PUBLIC Threads::Threads)
if(NEOCORTEX_SANITIZER STREQUAL "address-undefined")
  target_compile_options(neocortex_lmdb PRIVATE -fsanitize=address,undefined -fno-omit-frame-pointer)
elseif(NEOCORTEX_SANITIZER STREQUAL "thread")
  target_compile_options(neocortex_lmdb PRIVATE -fsanitize=thread -fno-omit-frame-pointer)
endif()

add_library(neocortex_roaring STATIC
  third_party/croaring/src/array_util.c
  third_party/croaring/src/bitset.c
  third_party/croaring/src/bitset_util.c
  third_party/croaring/src/isadetection.c
  third_party/croaring/src/memory.c
  third_party/croaring/src/roaring.c
  third_party/croaring/src/roaring64.c
  third_party/croaring/src/roaring_array.c
  third_party/croaring/src/roaring_priority_queue.c
  third_party/croaring/src/art/art.c
  third_party/croaring/src/containers/array.c
  third_party/croaring/src/containers/bitset.c
  third_party/croaring/src/containers/containers.c
  third_party/croaring/src/containers/convert.c
  third_party/croaring/src/containers/mixed_andnot.c
  third_party/croaring/src/containers/mixed_equal.c
  third_party/croaring/src/containers/mixed_intersection.c
  third_party/croaring/src/containers/mixed_negation.c
  third_party/croaring/src/containers/mixed_subset.c
  third_party/croaring/src/containers/mixed_union.c
  third_party/croaring/src/containers/mixed_xor.c
  third_party/croaring/src/containers/run.c
)
target_include_directories(neocortex_roaring SYSTEM PUBLIC
  third_party/croaring/include
)
target_include_directories(neocortex_roaring PRIVATE third_party/croaring/src)
target_compile_definitions(neocortex_roaring PUBLIC
  CROARING_COMPILER_SUPPORTS_AVX512=0
)
if(NEOCORTEX_SANITIZER STREQUAL "address-undefined")
  target_compile_options(neocortex_roaring PRIVATE -fsanitize=address,undefined -fno-omit-frame-pointer)
elseif(NEOCORTEX_SANITIZER STREQUAL "thread")
  target_compile_options(neocortex_roaring PRIVATE -fsanitize=thread -fno-omit-frame-pointer)
endif()

add_library(neocortex_proj STATIC
  src/proj/store.cc
  src/proj/beliefs/belief_store.cc
  src/proj/conversation_heads.cc
  src/proj/entity/entity_index.cc
  src/proj/vectors/vector_lane.cc
  src/proj/lexical/bm25.cc
  src/proj/ladder/temporal_ladder.cc
  src/proj/intent/intent_frame.cc
  src/proj/ledger/work_ledger.cc
)
target_include_directories(neocortex_proj PUBLIC src)
target_include_directories(neocortex_proj SYSTEM PUBLIC third_party/highway)
target_link_libraries(neocortex_proj PUBLIC neocortex_lmdb neocortex_log
                                      neocortex_schema neocortex_gate
                                      neocortex_roaring)
neocortex_target(neocortex_proj)

add_library(neocortex_compose STATIC src/compose/composer.cc)
target_include_directories(neocortex_compose PUBLIC src)
target_include_directories(neocortex_compose SYSTEM PUBLIC
  third_party/flatbuffers/include
)
target_link_libraries(neocortex_compose PUBLIC neocortex_proj)
neocortex_target(neocortex_compose)

add_library(neocortex_serve STATIC
  src/serve/protocol.cc
  src/serve/config.cc
  src/serve/actor_engine.cc
  src/serve/server.cc
)
target_include_directories(neocortex_serve PUBLIC src)
target_include_directories(neocortex_serve SYSTEM PUBLIC
  third_party/flatbuffers/include
)
target_link_libraries(neocortex_serve PUBLIC neocortex_core neocortex_compose
                                      PRIVATE crc32c)
neocortex_target(neocortex_serve)

add_executable(cortexd cmd/cortexd/main.cc)
target_link_libraries(cortexd PRIVATE neocortex_serve)
neocortex_target(cortexd)

add_executable(neocortex_projection_test test/projection_test.cc)
target_link_libraries(neocortex_projection_test PRIVATE neocortex_proj)
neocortex_target(neocortex_projection_test)
add_test(NAME projection_rebuild COMMAND neocortex_projection_test)

add_executable(neocortex_belief_store_test test/belief_store_test.cc)
target_link_libraries(neocortex_belief_store_test PRIVATE neocortex_proj)
neocortex_target(neocortex_belief_store_test)
add_test(NAME belief_store COMMAND neocortex_belief_store_test)

add_executable(neocortex_entity_index_test test/entity_index_test.cc)
target_link_libraries(neocortex_entity_index_test PRIVATE neocortex_proj)
neocortex_target(neocortex_entity_index_test)
add_test(NAME entity_index COMMAND neocortex_entity_index_test)

add_executable(neocortex_index_plane_test test/index_plane_test.cc)
target_link_libraries(neocortex_index_plane_test PRIVATE neocortex_proj)
neocortex_target(neocortex_index_plane_test)
add_test(NAME index_plane COMMAND neocortex_index_plane_test)

add_executable(neocortex_temporal_ladder_test test/temporal_ladder_test.cc)
target_link_libraries(neocortex_temporal_ladder_test PRIVATE neocortex_proj)
neocortex_target(neocortex_temporal_ladder_test)
add_test(NAME temporal_ladder COMMAND neocortex_temporal_ladder_test)

add_executable(neocortex_continuity_projection_test
  test/continuity_projection_test.cc
)
target_link_libraries(neocortex_continuity_projection_test PRIVATE neocortex_compose)
neocortex_target(neocortex_continuity_projection_test)
add_test(NAME continuity_projections COMMAND neocortex_continuity_projection_test)

add_executable(neocortex_composer_test test/composer_test.cc)
target_link_libraries(neocortex_composer_test PRIVATE neocortex_compose)
neocortex_target(neocortex_composer_test)
add_test(NAME activation_composer COMMAND neocortex_composer_test)

add_executable(neocortex_cortexd_test test/cortexd_test.cc)
target_link_libraries(neocortex_cortexd_test PRIVATE neocortex_serve)
neocortex_target(neocortex_cortexd_test)
add_test(NAME cortexd_real_socket
  COMMAND neocortex_cortexd_test $<TARGET_FILE:cortexd>
)

if(NEOCORTEX_FUZZING)
  set(NEOCORTEX_FUZZER_DIR toolchain/compiler-rt/lib/fuzzer)
  add_library(neocortex_fuzzer_runtime STATIC
    ${NEOCORTEX_FUZZER_DIR}/FuzzerCrossOver.cpp
    ${NEOCORTEX_FUZZER_DIR}/FuzzerDataFlowTrace.cpp
    ${NEOCORTEX_FUZZER_DIR}/FuzzerDriver.cpp
    ${NEOCORTEX_FUZZER_DIR}/FuzzerExtFunctionsDlsym.cpp
    ${NEOCORTEX_FUZZER_DIR}/FuzzerExtFunctionsWeak.cpp
    ${NEOCORTEX_FUZZER_DIR}/FuzzerExtraCounters.cpp
    ${NEOCORTEX_FUZZER_DIR}/FuzzerFork.cpp
    ${NEOCORTEX_FUZZER_DIR}/FuzzerIO.cpp
    ${NEOCORTEX_FUZZER_DIR}/FuzzerIOPosix.cpp
    ${NEOCORTEX_FUZZER_DIR}/FuzzerLoop.cpp
    ${NEOCORTEX_FUZZER_DIR}/FuzzerMain.cpp
    ${NEOCORTEX_FUZZER_DIR}/FuzzerMerge.cpp
    ${NEOCORTEX_FUZZER_DIR}/FuzzerMutate.cpp
    ${NEOCORTEX_FUZZER_DIR}/FuzzerSHA1.cpp
    ${NEOCORTEX_FUZZER_DIR}/FuzzerTracePC.cpp
    ${NEOCORTEX_FUZZER_DIR}/FuzzerUtil.cpp
    ${NEOCORTEX_FUZZER_DIR}/FuzzerUtilLinux.cpp
    ${NEOCORTEX_FUZZER_DIR}/FuzzerUtilPosix.cpp
  )
  target_include_directories(neocortex_fuzzer_runtime PRIVATE ${NEOCORTEX_FUZZER_DIR})
  target_compile_options(neocortex_fuzzer_runtime PRIVATE
    -Wall -Wextra -Werror -Wno-unused-parameter
    -fno-exceptions -fno-rtti -fno-omit-frame-pointer
  )
  add_executable(fuzz_frame fuzz/frame_fuzz.cc)
  target_link_libraries(fuzz_frame PRIVATE neocortex_log neocortex_fuzzer_runtime)
  neocortex_target(fuzz_frame)
  target_compile_options(fuzz_frame PRIVATE -fsanitize=fuzzer-no-link,address,undefined)
  target_link_options(fuzz_frame PRIVATE -fsanitize=address,undefined)

  add_executable(fuzz_mmr_state fuzz/mmr_state_fuzz.cc)
  target_link_libraries(fuzz_mmr_state PRIVATE neocortex_mmr neocortex_fuzzer_runtime)
  neocortex_target(fuzz_mmr_state)
  target_compile_options(fuzz_mmr_state PRIVATE -fsanitize=fuzzer-no-link,address,undefined)
  target_link_options(fuzz_mmr_state PRIVATE -fsanitize=address,undefined)

  add_executable(fuzz_unseal fuzz/unseal_fuzz.cc)
  target_link_libraries(fuzz_unseal PRIVATE neocortex_seal neocortex_sim
                                              neocortex_fuzzer_runtime)
  neocortex_target(fuzz_unseal)
  target_compile_options(fuzz_unseal PRIVATE -fsanitize=fuzzer-no-link,address,undefined)
  target_link_options(fuzz_unseal PRIVATE -fsanitize=address,undefined)

  add_executable(fuzz_schema fuzz/schema_fuzz.cc)
  target_link_libraries(fuzz_schema PRIVATE neocortex_schema neocortex_fuzzer_runtime)
  neocortex_target(fuzz_schema)
  target_compile_options(fuzz_schema PRIVATE -fsanitize=fuzzer-no-link,address,undefined)
  target_link_options(fuzz_schema PRIVATE -fsanitize=address,undefined)

  add_executable(fuzz_protocol_frame fuzz/protocol_frame_fuzz.cc)
  target_link_libraries(fuzz_protocol_frame PRIVATE neocortex_serve
                                                    neocortex_fuzzer_runtime)
  neocortex_target(fuzz_protocol_frame)
  target_compile_options(fuzz_protocol_frame PRIVATE
    -fsanitize=fuzzer-no-link,address,undefined
  )
  target_link_options(fuzz_protocol_frame PRIVATE -fsanitize=address,undefined)

  add_executable(fuzz_query_input fuzz/query_input_fuzz.cc)
  target_link_libraries(fuzz_query_input PRIVATE neocortex_serve
                                                neocortex_fuzzer_runtime)
  neocortex_target(fuzz_query_input)
  target_compile_options(fuzz_query_input PRIVATE
    -fsanitize=fuzzer-no-link,address,undefined
  )
  target_link_options(fuzz_query_input PRIVATE -fsanitize=address,undefined)
endif()
