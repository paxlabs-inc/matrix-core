#include <cstdio>
#include <filesystem>
#include <string_view>
#include <utility>

#include "serve/config.h"
#include "serve/server.h"

namespace {

int Fail(const char* stage, const neocortex::Error& error) {
  std::fprintf(stderr, "cortexd: %s: code=%u system=%d lsn=%llu offset=%llu\n",
               stage, static_cast<unsigned int>(error.code),
               error.system_error,
               static_cast<unsigned long long>(error.lsn),
               static_cast<unsigned long long>(error.offset));
  return 1;
}

}  // namespace

int main(int argc, char** argv) {
  if (argc != 3 || std::string_view(argv[1]) != "--config") {
    std::fputs("usage: cortexd --config <path>\n", stderr);
    return 2;
  }
  auto config = neocortex::serve::LoadServerConfig(std::filesystem::path(argv[2]));
  if (!config) {
    return Fail("config", config.error());
  }
  auto server = neocortex::serve::Server::Open(std::move(*config));
  if (!server) {
    return Fail("open", server.error());
  }
  auto run = (*server)->Run();
  return run ? 0 : Fail("run", run.error());
}
