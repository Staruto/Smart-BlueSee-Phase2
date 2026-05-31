#include <Arduino.h>
#include <WiFi.h>
#include <WiFiUdp.h>
#include <driver/i2s.h>

// ========== Wi-Fi & UDP 配置 ==========
const char* ssid     = "YOUR_WIFI_SSID";
const char* password = "YOUR_WIFI_PASSWORD";

// 对应云端 Go 服务器的 IP 和 UDP 监听端口
const char* serverIP = "192.168.x.x"; 
const int serverUDPPort = 5000;      
const int localUDPPort = 5000;       

WiFiUDP udp;

// ========== I2S 参数 ==========
#define I2S_PORT_IN   I2S_NUM_0 // 用于麦克风输入
#define I2S_PORT_OUT  I2S_NUM_1 // 用于扬声器输出

// INMP441 麦克风引脚 (示例)
#define I2S_IN_BCLK   4
#define I2S_IN_LRC    5
#define I2S_IN_DOUT   6

// MAX98357A 扬声器引脚 (示例)
#define I2S_OUT_BCLK  7
#define I2S_OUT_LRC   8
#define I2S_OUT_DIN   9

// 采用 8kHz 采样率，配合 G.711 (PCMU) 刚好合适
#define SAMPLE_RATE   8000

// ========== G.711 PCMU 编解码 ==========
#define BIAS 0x84
#define CLIP 32635

static uint8_t encode_ulaw(int16_t sample) {
    static const int exp_lut[256] = {
        0,0,1,1,2,2,2,2,3,3,3,3,3,3,3,3,
        4,4,4,4,4,4,4,4,4,4,4,4,4,4,4,4,
        5,5,5,5,5,5,5,5,5,5,5,5,5,5,5,5,
        5,5,5,5,5,5,5,5,5,5,5,5,5,5,5,5,
        6,6,6,6,6,6,6,6,6,6,6,6,6,6,6,6,
        6,6,6,6,6,6,6,6,6,6,6,6,6,6,6,6,
        6,6,6,6,6,6,6,6,6,6,6,6,6,6,6,6,
        6,6,6,6,6,6,6,6,6,6,6,6,6,6,6,6,
        7,7,7,7,7,7,7,7,7,7,7,7,7,7,7,7,
        7,7,7,7,7,7,7,7,7,7,7,7,7,7,7,7,
        7,7,7,7,7,7,7,7,7,7,7,7,7,7,7,7,
        7,7,7,7,7,7,7,7,7,7,7,7,7,7,7,7,
        7,7,7,7,7,7,7,7,7,7,7,7,7,7,7,7,
        7,7,7,7,7,7,7,7,7,7,7,7,7,7,7,7,
        7,7,7,7,7,7,7,7,7,7,7,7,7,7,7,7,
        7,7,7,7,7,7,7,7,7,7,7,7,7,7,7,7
    };
    int sign, exponent, mantissa;
    uint8_t ulawbyte;

    sign = (sample >> 8) & 0x80;
    if (sign != 0) sample = -sample;
    if (sample > CLIP) sample = CLIP;

    sample = sample + BIAS;
    exponent = exp_lut[(sample >> 7) & 0xFF];
    mantissa = (sample >> (exponent + 3)) & 0x0F;
    ulawbyte = ~(sign | (exponent << 4) | mantissa);

    if (ulawbyte == 0) ulawbyte = 0x02;

    return ulawbyte;
}

static int16_t decode_ulaw(uint8_t ulawbyte) {
    static const int exp_lut[8] = {0, 132, 396, 924, 1980, 4092, 8316, 16764};
    int sign, exponent, mantissa, sample;

    ulawbyte = ~ulawbyte;
    sign = (ulawbyte & 0x80);
    exponent = (ulawbyte >> 4) & 0x07;
    mantissa = ulawbyte & 0x0F;

    sample = exp_lut[exponent] + (mantissa << (exponent + 3));
    if (sign != 0) sample = -sample;

    return sample;
}

void setupWiFi() {
    Serial.print("Connecting to WiFi");
    WiFi.begin(ssid, password);
    while (WiFi.status() != WL_CONNECTED) {
        delay(500);
        Serial.print(".");
    }
    Serial.println("\nWiFi connected.");
    Serial.print("IP Address: ");
    Serial.println(WiFi.localIP());

    udp.begin(localUDPPort);
}

void setupI2S_In() {
    i2s_config_t i2s_config = {
        .mode = (i2s_mode_t)(I2S_MODE_MASTER | I2S_MODE_RX),
        .sample_rate = SAMPLE_RATE,
        .bits_per_sample = I2S_BITS_PER_SAMPLE_16BIT,
        .channel_format = I2S_CHANNEL_FMT_ONLY_LEFT,
        .communication_format = I2S_COMM_FORMAT_STAND_I2S,
        .intr_alloc_flags = ESP_INTR_FLAG_LEVEL1,
        .dma_buf_count = 8,
        .dma_buf_len = 256,
        .use_apll = false,
        .tx_desc_auto_clear = false,
        .fixed_mclk = 0
    };

    i2s_pin_config_t pin_config = {
        .bck_io_num = I2S_IN_BCLK,
        .ws_io_num = I2S_IN_LRC,
        .data_out_num = I2S_PIN_NO_CHANGE,
        .data_in_num = I2S_IN_DOUT
    };

    i2s_driver_install(I2S_PORT_IN, &i2s_config, 0, NULL);
    i2s_set_pin(I2S_PORT_IN, &pin_config);
}

void setupI2S_Out() {
    i2s_config_t i2s_config = {
        .mode = (i2s_mode_t)(I2S_MODE_MASTER | I2S_MODE_TX),
        .sample_rate = SAMPLE_RATE,
        .bits_per_sample = I2S_BITS_PER_SAMPLE_16BIT,
        .channel_format = I2S_CHANNEL_FMT_ONLY_LEFT,
        .communication_format = I2S_COMM_FORMAT_STAND_I2S,
        .intr_alloc_flags = ESP_INTR_FLAG_LEVEL1,
        .dma_buf_count = 8,
        .dma_buf_len = 256,
        .use_apll = false,
        .tx_desc_auto_clear = true,
        .fixed_mclk = 0
    };

    i2s_pin_config_t pin_config = {
        .bck_io_num = I2S_OUT_BCLK,
        .ws_io_num = I2S_OUT_LRC,
        .data_out_num = I2S_OUT_DIN,
        .data_in_num = I2S_PIN_NO_CHANGE
    };

    i2s_driver_install(I2S_PORT_OUT, &i2s_config, 0, NULL);
    i2s_set_pin(I2S_PORT_OUT, &pin_config);
}

void setup() {
    Serial.begin(115200);
    delay(1000);

    setupWiFi();
    setupI2S_In();
    setupI2S_Out();

    Serial.println("System Initialized...");
}

void loop() {
    // ---- 1. 读取麦克风声音，压缩成 PCMU 后发送 ----
    int16_t in_buffer[256];
    size_t bytes_read = 0;
    
    // DMA读取块
    i2s_read(I2S_PORT_IN, &in_buffer, sizeof(in_buffer), &bytes_read, portMAX_DELAY);
    
    if (bytes_read > 0) {
        int samples = bytes_read / sizeof(int16_t);
        uint8_t pcmu_buffer[256];
        
        for (int i = 0; i < samples; i++) {
            pcmu_buffer[i] = encode_ulaw(in_buffer[i]);
        }
        
        udp.beginPacket(serverIP, serverUDPPort);
        udp.write(pcmu_buffer, samples);
        udp.endPacket();
    }

    // ---- 2. 从云端接收 PCMU 声音，解压成 PCM 后写入扬声器 ----
    int packetSize = udp.parsePacket();
    if (packetSize > 0) {
        uint8_t udp_buffer[1024];
        int len = udp.read(udp_buffer, sizeof(udp_buffer));
        
        if (len > 0) {
            int16_t out_buffer[1024];
            
            for (int i = 0; i < len; i++) {
                out_buffer[i] = decode_ulaw(udp_buffer[i]);
            }
            
            size_t bytes_written = 0;
            // 写入 I2S DMA 进行发声 (注意写入的字节数是样本数 * int16_t大小)
            i2s_write(I2S_PORT_OUT, out_buffer, len * sizeof(int16_t), &bytes_written, portMAX_DELAY);
        }
    }
}
