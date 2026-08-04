set(CMAKE_SYSTEM_NAME Linux)
set(CMAKE_SYSTEM_PROCESSOR aarch64)

get_filename_component(NEOCORTEX_ROOT "${CMAKE_CURRENT_LIST_DIR}/.." ABSOLUTE)
set(NEOCORTEX_SYSROOT "${NEOCORTEX_ROOT}/toolchain/sysroots/aarch64-linux-gnu")

set(CMAKE_C_COMPILER "/usr/bin/clang-18" CACHE FILEPATH "" FORCE)
set(CMAKE_CXX_COMPILER "/usr/bin/clang++-18" CACHE FILEPATH "" FORCE)
set(CMAKE_C_COMPILER_TARGET aarch64-linux-gnu)
set(CMAKE_CXX_COMPILER_TARGET aarch64-linux-gnu)
set(CMAKE_AR "/usr/bin/llvm-ar-18" CACHE FILEPATH "" FORCE)
set(CMAKE_RANLIB "/usr/bin/llvm-ranlib-18" CACHE FILEPATH "" FORCE)
set(CMAKE_LINKER_TYPE LLD)
set(CMAKE_SYSROOT "${NEOCORTEX_SYSROOT}")
set(CMAKE_TRY_COMPILE_TARGET_TYPE STATIC_LIBRARY)

set(NEOCORTEX_CXX_INCLUDE "${NEOCORTEX_SYSROOT}/usr/lib/llvm-18/include/c++/v1")
set(NEOCORTEX_RESOURCE_DIR "${NEOCORTEX_SYSROOT}/usr/lib/llvm-18/lib/clang/18")
set(NEOCORTEX_RUNTIME_DIR "${NEOCORTEX_SYSROOT}/usr/lib/llvm-18/lib")
set(NEOCORTEX_MULTIARCH_DIR "${NEOCORTEX_SYSROOT}/usr/lib/aarch64-linux-gnu")

set(CMAKE_C_FLAGS_INIT "-resource-dir=${NEOCORTEX_RESOURCE_DIR}")
set(CMAKE_CXX_FLAGS_INIT "-nostdinc++ -isystem ${NEOCORTEX_CXX_INCLUDE} -resource-dir=${NEOCORTEX_RESOURCE_DIR}")
set(CMAKE_EXE_LINKER_FLAGS_INIT "-fuse-ld=lld-18 -nostdlib++ -L${NEOCORTEX_RUNTIME_DIR} -L${NEOCORTEX_MULTIARCH_DIR} -lc++ -lc++abi -lunwind --rtlib=compiler-rt --unwindlib=libunwind -resource-dir=${NEOCORTEX_RESOURCE_DIR}")
