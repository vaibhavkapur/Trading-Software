#include "inference_kernel.hpp"

#include <algorithm>
#include <chrono>
#include <cmath>
#include <cstdint>
#include <fstream>
#include <iomanip>
#include <iostream>
#include <string>
#include <thread>
#include <vector>

namespace {

using Clock = std::chrono::steady_clock;

struct LatencyStats {
    std::size_t count = 0;
    double mean_us = 0.0;
    double min_us = 0.0;
    double p50_us = 0.0;
    double p95_us = 0.0;
    double p99_us = 0.0;
    double max_us = 0.0;
};

LatencyStats compute_stats(std::vector<double>& samples_us) {
    LatencyStats s;
    s.count = samples_us.size();
    if (samples_us.empty()) {
        return s;
    }
    std::sort(samples_us.begin(), samples_us.end());
    double sum = 0.0;
    for (double v : samples_us) {
        sum += v;
    }
    auto pct = [&](double p) -> double {
        const double idx = p * static_cast<double>(samples_us.size() - 1);
        const auto lo = static_cast<std::size_t>(std::floor(idx));
        const auto hi = static_cast<std::size_t>(std::ceil(idx));
        if (lo == hi) {
            return samples_us[lo];
        }
        const double frac = idx - static_cast<double>(lo);
        return samples_us[lo] * (1.0 - frac) + samples_us[hi] * frac;
    };
    s.mean_us = sum / static_cast<double>(samples_us.size());
    s.min_us = samples_us.front();
    s.p50_us = pct(0.50);
    s.p95_us = pct(0.95);
    s.p99_us = pct(0.99);
    s.max_us = samples_us.back();
    return s;
}

void print_stats_json(const LatencyStats& s) {
    std::cout << std::fixed << std::setprecision(4)
              << "{\"count\":" << s.count
              << ",\"mean_us\":" << s.mean_us
              << ",\"min_us\":" << s.min_us
              << ",\"p50_us\":" << s.p50_us
              << ",\"p95_us\":" << s.p95_us
              << ",\"p99_us\":" << s.p99_us
              << ",\"max_us\":" << s.max_us << "}";
}

}  // namespace

// Native C++20 microbenchmark measuring pure C++20 kernel latency without Python boundary overhead.
// Expects path to weights.bin matching the default 8->16(relu)->8(tanh)->1(linear) v1.0.0 architecture.
int main(int argc, char** argv) {
    if (argc < 2) {
        std::cerr << "Usage: bench_native <weights.bin> [single_iters] [batch_iters] [threads]\n";
        return 2;
    }
    const std::string weights_path = argv[1];
    const int single_iters = argc >= 3 ? std::stoi(argv[2]) : 20000;
    const int batch_iters = argc >= 4 ? std::stoi(argv[3]) : 1000;
    const int num_threads = argc >= 5 ? std::stoi(argv[4]) : 4;
    constexpr int batch_size = 64;

    const auto t_cold_start = Clock::now();
    std::ifstream ifs(weights_path, std::ios::binary | std::ios::ate);
    if (!ifs) {
        std::cerr << "Failed to open weights file: " << weights_path << "\n";
        return 1;
    }
    const std::streamsize byte_size = ifs.tellg();
    ifs.seekg(0, std::ios::beg);
    if (byte_size <= 0 || (byte_size % 8) != 0) {
        std::cerr << "Invalid weights byte size\n";
        return 1;
    }
    std::vector<double> weights(static_cast<std::size_t>(byte_size / 8));
    if (!ifs.read(reinterpret_cast<char*>(weights.data()), byte_size)) {
        std::cerr << "Failed to read weights file\n";
        return 1;
    }

    const int32_t num_features = 8;
    const double means[8] = {0.0, 0.0, 2.5, 0.0, 0.0, 0.0, 8.5, 0.0};
    const double stds[8] = {0.42, 0.35, 1.0, 2.0, 1.5, 0.4, 3.0, 0.6};
    const double mins[8] = {-1.0, -1.0, 0.0, -200.0, -50.0, -1.0, 0.0, -10.0};
    const double maxs[8] = {1.0, 1.0, 500.0, 200.0, 50.0, 1.0, 1000.0, 10.0};
    const double clips[8] = {5.0, 5.0, 5.0, 5.0, 5.0, 5.0, 5.0, 5.0};

    // Infer v1 (289 params) vs v2 (833 params)
    std::vector<int32_t> in_dims;
    std::vector<int32_t> out_dims;
    std::vector<int32_t> acts;
    if (weights.size() == 289) {
        in_dims = {8, 16, 8};
        out_dims = {16, 8, 1};
        acts = {ML_ACT_RELU, ML_ACT_TANH, ML_ACT_LINEAR};
    } else if (weights.size() == 833) {
        in_dims = {8, 32, 16};
        out_dims = {32, 16, 1};
        acts = {ML_ACT_RELU, ML_ACT_RELU, ML_ACT_LINEAR};
    } else {
        std::cerr << "Unsupported weights count for bench_native: " << weights.size() << "\n";
        return 1;
    }

    MlNativeModel* model = ml_engine_create_model(
        num_features, means, stds, mins, maxs, clips,
        static_cast<int32_t>(in_dims.size()),
        in_dims.data(), out_dims.data(), acts.data(),
        weights.data(), static_cast<int64_t>(weights.size())
    );
    if (!model) {
        std::cerr << "ml_engine_create_model failed\n";
        return 1;
    }

    const double sample_input[8] = {0.35, 0.22, 2.1, 1.4, 1.2, 0.18, 8.9, 0.45};
    double cold_out = 0.0;
    const int32_t cold_st = ml_engine_score_one(model, sample_input, num_features, &cold_out);
    const auto t_cold_end = Clock::now();
    if (cold_st != ML_ENGINE_OK) {
        std::cerr << "Cold inference failed with status " << cold_st << "\n";
        ml_engine_destroy_model(model);
        return 1;
    }
    const double cold_start_us =
        std::chrono::duration<double, std::micro>(t_cold_end - t_cold_start).count();

    // Warmup
    volatile double sink = 0.0;
    for (int i = 0; i < 500; ++i) {
        double y = 0.0;
        ml_engine_score_one(model, sample_input, num_features, &y);
        sink += y;
    }

    // Warm single inference
    std::vector<double> single_lat_us;
    single_lat_us.reserve(static_cast<std::size_t>(single_iters));
    for (int i = 0; i < single_iters; ++i) {
        double y = 0.0;
        const auto t0 = Clock::now();
        ml_engine_score_one(model, sample_input, num_features, &y);
        const auto t1 = Clock::now();
        sink += y;
        single_lat_us.push_back(std::chrono::duration<double, std::micro>(t1 - t0).count());
    }

    // Warm batch inference
    std::vector<double> batch_in(static_cast<std::size_t>(batch_size * num_features));
    for (int b = 0; b < batch_size; ++b) {
        for (int f = 0; f < num_features; ++f) {
            batch_in[static_cast<std::size_t>(b * num_features + f)] = sample_input[f];
        }
    }
    std::vector<double> batch_out(static_cast<std::size_t>(batch_size), 0.0);
    std::vector<double> batch_lat_us;
    std::vector<double> batch_per_item_us;
    batch_lat_us.reserve(static_cast<std::size_t>(batch_iters));
    batch_per_item_us.reserve(static_cast<std::size_t>(batch_iters));
    for (int i = 0; i < batch_iters; ++i) {
        const auto t0 = Clock::now();
        ml_engine_score_batch(model, batch_in.data(), batch_size, num_features, batch_out.data());
        const auto t1 = Clock::now();
        sink += batch_out[0];
        const double dt = std::chrono::duration<double, std::micro>(t1 - t0).count();
        batch_lat_us.push_back(dt);
        batch_per_item_us.push_back(dt / static_cast<double>(batch_size));
    }

    // Concurrent multi-threaded inference
    const int per_thread_iters = std::max(1000, single_iters / std::max(1, num_threads));
    std::vector<std::vector<double>> thread_lats(static_cast<std::size_t>(num_threads));
    std::vector<std::thread> threads;
    const auto t_conc_start = Clock::now();
    for (int t = 0; t < num_threads; ++t) {
        thread_lats[static_cast<std::size_t>(t)].reserve(static_cast<std::size_t>(per_thread_iters));
        threads.emplace_back([&, t]() {
            auto& lats = thread_lats[static_cast<std::size_t>(t)];
            double local_sink = 0.0;
            for (int i = 0; i < per_thread_iters; ++i) {
                double y = 0.0;
                const auto t0 = Clock::now();
                ml_engine_score_one(model, sample_input, num_features, &y);
                const auto t1 = Clock::now();
                local_sink += y;
                lats.push_back(std::chrono::duration<double, std::micro>(t1 - t0).count());
            }
            if (local_sink == 1e300) {
                std::cerr << "";
            }
        });
    }
    for (auto& th : threads) {
        th.join();
    }
    const auto t_conc_end = Clock::now();
    const double conc_wall_s =
        std::chrono::duration<double>(t_conc_end - t_conc_start).count();
    std::vector<double> all_conc_lats;
    all_conc_lats.reserve(static_cast<std::size_t>(num_threads * per_thread_iters));
    for (const auto& v : thread_lats) {
        all_conc_lats.insert(all_conc_lats.end(), v.begin(), v.end());
    }
    const double conc_qps =
        conc_wall_s > 0.0 ? static_cast<double>(all_conc_lats.size()) / conc_wall_s : 0.0;

    const auto single_stats = compute_stats(single_lat_us);
    const auto batch_stats = compute_stats(batch_lat_us);
    const auto batch_item_stats = compute_stats(batch_per_item_us);
    const auto conc_stats = compute_stats(all_conc_lats);

    std::cout << std::fixed << std::setprecision(4)
              << "{\"kernel_version\":\"" << ml_engine_version() << "\""
              << ",\"cold_start_us\":" << cold_start_us
              << ",\"warm_single\":";
    print_stats_json(single_stats);
    std::cout << ",\"warm_batch_64\":";
    print_stats_json(batch_stats);
    std::cout << ",\"warm_batch_64_per_item\":";
    print_stats_json(batch_item_stats);
    std::cout << ",\"concurrent\":{\"threads\":" << num_threads
              << ",\"throughput_qps\":" << conc_qps
              << ",\"latency\":";
    print_stats_json(conc_stats);
    std::cout << "},\"sink_guard\":" << sink << "}\n";

    ml_engine_destroy_model(model);
    return 0;
}
