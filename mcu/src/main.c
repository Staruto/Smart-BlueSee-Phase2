#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "freertos/event_groups.h"
#include "esp_system.h"
#include "esp_log.h"
#include "nvs_flash.h"
#include "esp_event.h"
#include "esp_netif.h"
#include "esp_wifi.h"
#include "driver/i2s.h"
#include "driver/gpio.h"
#include <sys/socket.h>
#include <netinet/in.h>
#include <arpa/inet.h>
#include <unistd.h>

static const char *TAG = "udp_pcmu_espidf";

// --- Hardware pins (from your mapping) ---
#define I2S_PORT      I2S_NUM_0
#define I2S_BCLK      7
#define I2S_WS        2
#define I2S_DATA_IN   3
#define I2S_DATA_OUT  1
#define SWITCH_GPIO   4

// --- Network / UDP ---
#define WIFI_SSID      "yuhao"
#define WIFI_PASS      "12345678"
#define UDP_SERVER_IP  "192.168.137.101"
#define UDP_SERVER_PORT 5000
#define UDP_LOCAL_PORT  5000

// --- Audio ---
#define SAMPLE_RATE    8000
#define I2S_DMA_BUF_COUNT 8
#define I2S_DMA_BUF_LEN   256

static EventGroupHandle_t s_wifi_event_group;
const int WIFI_CONNECTED_BIT = BIT0;

static void wifi_event_handler(void* arg, esp_event_base_t event_base,
                               int32_t event_id, void* event_data) {
    if (event_base == WIFI_EVENT && event_id == WIFI_EVENT_STA_START) {
        esp_wifi_connect();
    } else if (event_base == IP_EVENT && event_id == IP_EVENT_STA_GOT_IP) {
        xEventGroupSetBits(s_wifi_event_group, WIFI_CONNECTED_BIT);
    } else if (event_base == WIFI_EVENT && event_id == WIFI_EVENT_STA_DISCONNECTED) {
        esp_wifi_connect();
        xEventGroupClearBits(s_wifi_event_group, WIFI_CONNECTED_BIT);
    }
}

static void wifi_init_sta(void) {
    s_wifi_event_group = xEventGroupCreate();
    ESP_ERROR_CHECK(esp_netif_init());
    ESP_ERROR_CHECK(esp_event_loop_create_default());
    esp_netif_create_default_wifi_sta();

    wifi_init_config_t cfg = WIFI_INIT_CONFIG_DEFAULT();
    ESP_ERROR_CHECK(esp_wifi_init(&cfg));

    ESP_ERROR_CHECK(esp_event_handler_instance_register(WIFI_EVENT,
                                                        ESP_EVENT_ANY_ID,
                                                        &wifi_event_handler,
                                                        NULL,
                                                        NULL));
    ESP_ERROR_CHECK(esp_event_handler_instance_register(IP_EVENT,
                                                        IP_EVENT_STA_GOT_IP,
                                                        &wifi_event_handler,
                                                        NULL,
                                                        NULL));

    wifi_config_t wifi_config = {0};
    strncpy((char*)wifi_config.sta.ssid, WIFI_SSID, sizeof(wifi_config.sta.ssid)-1);
    strncpy((char*)wifi_config.sta.password, WIFI_PASS, sizeof(wifi_config.sta.password)-1);

    ESP_ERROR_CHECK(esp_wifi_set_mode(WIFI_MODE_STA));
    ESP_ERROR_CHECK(esp_wifi_set_config(WIFI_IF_STA, &wifi_config));
    ESP_ERROR_CHECK(esp_wifi_start());
    ESP_ERROR_CHECK(esp_wifi_set_ps(WIFI_PS_NONE));

    ESP_LOGI(TAG, "wifi_init_sta finished. Connecting to SSID:%s", WIFI_SSID);
}

static void i2s_init(void) {
    i2s_config_t i2s_config = {
        .mode = (i2s_mode_t)(I2S_MODE_MASTER | I2S_MODE_TX | I2S_MODE_RX),
        .sample_rate = SAMPLE_RATE,
        .bits_per_sample = I2S_BITS_PER_SAMPLE_32BIT,
        .channel_format = I2S_CHANNEL_FMT_ONLY_LEFT,
        .communication_format = I2S_COMM_FORMAT_I2S_MSB,
        .intr_alloc_flags = ESP_INTR_FLAG_LEVEL1,
        .dma_buf_count = I2S_DMA_BUF_COUNT,
        .dma_buf_len = I2S_DMA_BUF_LEN,
        .use_apll = false,
        .tx_desc_auto_clear = true,
    };

    i2s_pin_config_t pin_config = {
        .mck_io_num = I2S_PIN_NO_CHANGE,
        .bck_io_num = I2S_BCLK,
        .ws_io_num = I2S_WS,
        .data_out_num = I2S_DATA_OUT,
        .data_in_num = I2S_DATA_IN,
    };

    ESP_ERROR_CHECK(i2s_driver_install(I2S_PORT, &i2s_config, 0, NULL));
    ESP_ERROR_CHECK(i2s_set_pin(I2S_PORT, &pin_config));
    ESP_ERROR_CHECK(i2s_zero_dma_buffer(I2S_PORT));
}

// G.711 u-law
#define ULAW_BIAS 0x84
#define ULAW_CLIP 32635

static uint8_t encode_ulaw(int16_t sample) {
    static const int exp_lut[256] = { /* omitted init for brevity, we'll reuse standard table */
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
    if (sample > ULAW_CLIP) sample = ULAW_CLIP;

    sample = sample + ULAW_BIAS;
    exponent = exp_lut[(sample >> 7) & 0xFF];
    mantissa = (sample >> (exponent + 3)) & 0x0F;
    ulawbyte = ~(sign | (exponent << 4) | mantissa);

    if (ulawbyte == 0) ulawbyte = 0x02;

    return ulawbyte;
}

static int16_t decode_ulaw(uint8_t ulawbyte) {
    static const int exp_lut[8] = {0,132,396,924,1980,4092,8316,16764};
    int sign, exponent, mantissa, sample;

    ulawbyte = ~ulawbyte;
    sign = (ulawbyte & 0x80);
    exponent = (ulawbyte >> 4) & 0x07;
    mantissa = ulawbyte & 0x0F;

    sample = exp_lut[exponent] + (mantissa << (exponent + 3));
    if (sign != 0) sample = -sample;
    return (int16_t)sample;
}

// Always enable recording (no GPIO switch in this build)


static void udp_task(void *arg) {
    (void)arg;
    int sock = -1;
    struct sockaddr_in server_addr = {0};
    struct sockaddr_in local_addr = {0};
    sock = socket(AF_INET, SOCK_DGRAM, IPPROTO_UDP);
    if (sock < 0) {
        ESP_LOGE(TAG, "socket failed");
        vTaskDelete(NULL);
        return;
    }

    local_addr.sin_family = AF_INET;
    local_addr.sin_addr.s_addr = htonl(INADDR_ANY);
    local_addr.sin_port = htons(UDP_LOCAL_PORT);
    bind(sock, (struct sockaddr *)&local_addr, sizeof(local_addr));

    server_addr.sin_family = AF_INET;
    server_addr.sin_port = htons(UDP_SERVER_PORT);
    inet_aton(UDP_SERVER_IP, &server_addr.sin_addr);

    uint8_t *i2s_read_buf = (uint8_t *)malloc(4096);
    uint8_t rx_buf[2048];

    while (1) {
        size_t bytes_read = 0;
        esp_err_t r = i2s_read(I2S_PORT, i2s_read_buf, 4096, &bytes_read, pdMS_TO_TICKS(200));
        if (r == ESP_OK && bytes_read > 0) {
            size_t samples = bytes_read / sizeof(int32_t);
            if (samples > 2048) samples = 2048;
            uint8_t ulaw_buf[2048];
            int32_t *s32 = (int32_t *)i2s_read_buf;
            for (size_t i = 0; i < samples; i++) {
                int16_t s16 = (int16_t)(s32[i] >> 16);
                ulaw_buf[i] = encode_ulaw(s16);
            }
            sendto(sock, ulaw_buf, samples, 0, (struct sockaddr *)&server_addr, sizeof(server_addr));
        }

        // Use MSG_DONTWAIT to prevent hanging if no audio is received from PC
        ssize_t len = recvfrom(sock, rx_buf, sizeof(rx_buf), MSG_DONTWAIT, NULL, NULL);
        if (len > 0) {
            // decode and write to I2S
            int16_t pcm16[2048];
            size_t out_samples = 0;
            for (ssize_t i = 0; i < len; i++) {
                pcm16[out_samples++] = decode_ulaw(rx_buf[i]);
            }
            // convert to i2s 32-bit samples
            int32_t i2s_out[2048];
            for (size_t i = 0; i < out_samples; i++) {
                i2s_out[i] = ((int32_t)pcm16[i]) << 16;
            }
            size_t bytes_written = 0;
            i2s_write(I2S_PORT, i2s_out, out_samples * sizeof(int32_t), &bytes_written, pdMS_TO_TICKS(200));
        }
    }

    close(sock);
    free(i2s_read_buf);
    vTaskDelete(NULL);
}

void app_main(void) {
    esp_err_t ret = nvs_flash_init();
    if (ret == ESP_ERR_NVS_NO_FREE_PAGES || ret == ESP_ERR_NVS_NEW_VERSION_FOUND) {
        ESP_ERROR_CHECK(nvs_flash_erase());
        ret = nvs_flash_init();
    }
    ESP_ERROR_CHECK(ret);

    wifi_init_sta();
    i2s_init();

    // wait for wifi
    xEventGroupWaitBits(s_wifi_event_group, WIFI_CONNECTED_BIT, false, true, portMAX_DELAY);
    ESP_LOGI(TAG, "connected to WiFi, app started");

    // Start UDP task only after WiFi is connected
    xTaskCreate(&udp_task, "udp_audio", 8192, NULL, 6, NULL);
}
