CXX ?= clang++
CXXFLAGS ?= -std=c++20 -O3 -DNDEBUG -Wall -Wextra -Wpedantic
UNAME_S := $(shell uname -s)

BUILD_DIR ?= build

ifeq ($(UNAME_S),Darwin)
    SHARED_EXT := dylib
    SHARED_FLAGS := -dynamiclib -fPIC
else
    SHARED_EXT := so
    SHARED_FLAGS := -shared -fPIC
endif

LIB_TARGET := $(BUILD_DIR)/libml_inference_kernel.$(SHARED_EXT)
BENCH_TARGET := $(BUILD_DIR)/bench_native

.PHONY: all clean

all: $(LIB_TARGET) $(BENCH_TARGET)

$(BUILD_DIR):
	mkdir -p $(BUILD_DIR)

$(LIB_TARGET): inference_kernel.cpp inference_kernel.hpp | $(BUILD_DIR)
	$(CXX) $(CXXFLAGS) $(SHARED_FLAGS) inference_kernel.cpp -o $(LIB_TARGET)

$(BENCH_TARGET): bench_native.cpp inference_kernel.cpp inference_kernel.hpp | $(BUILD_DIR)
	$(CXX) $(CXXFLAGS) -pthread bench_native.cpp inference_kernel.cpp -o $(BENCH_TARGET)

clean:
	rm -rf $(BUILD_DIR)
