package main

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	requestURL  = "speed.cloudflare.com/cdn-cgi/trace" // 请求trace URL
	timeout     = 5 * time.Second                      // 超时时间
	maxDuration = 5 * time.Second                      // 最大持续时间
)

var (
	File         = flag.String("file", "ip.txt", "IP地址文件名称")                                   // IP地址文件名称
	outFile      = flag.String("outfile", "ip.csv", "输出文件名称")                                  // 输出文件名称
	defaultPort  = flag.Int("port", 443, "端口")                                                 // 端口
	maxThreads   = flag.Int("max", 100, "并发请求最大协程数")                                           // 最大协程数
	speedTest    = flag.Int("speedtest", 5, "下载测速协程数量,设为0禁用测速")                                // 下载测速协程数量
	speedTestURL = flag.String("url", "speed.cloudflare.com/__down?bytes=500000000", "测速文件地址") // 测速文件地址
	enableTLS    = flag.Bool("tls", true, "是否启用TLS")                                           // TLS是否启用
)

type result struct {
	ip          string        // IP地址
	port        int           // 端口
	latency     string        // 延迟
	tcpDuration time.Duration // TCP请求延迟
}

type speedtestresult struct {
	result
	downloadSpeed float64 // 下载速度
}

// 尝试提升文件描述符的上限
func increaseMaxOpenFiles() {
	fmt.Println("正在尝试提升文件描述符的上限...")
	cmd := exec.Command("bash", "-c", "ulimit -n 10000")
	_, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Printf("提升文件描述符上限时出现错误: %v\n", err)
	} else {
		fmt.Printf("文件描述符上限已提升!\n")
	}
}

func main() {
	flag.Parse()

	startTime := time.Now()
	osType := runtime.GOOS
	if osType == "linux" {
		increaseMaxOpenFiles()
	}

	ips, err := readIPs(*File)
	if err != nil {
		fmt.Printf("无法从文件中读取 IP: %v\n", err)
		return
	}

	var wg sync.WaitGroup
	wg.Add(len(ips))

	resultChan := make(chan result, len(ips))

	thread := make(chan struct{}, *maxThreads)

	var count int
	total := len(ips)

	for _, ip := range ips {
		thread <- struct{}{}
		go func(ip string) {
			defer func() {
				<-thread
				wg.Done()
				count++
				percentage := float64(count) / float64(total) * 100
				fmt.Printf("已完成: %d 总数: %d 已完成: %.2f%%\r", count, total, percentage)
				if count == total {
					fmt.Printf("已完成: %d 总数: %d 已完成: %.2f%%\n", count, total, percentage)
				}
			}()

			dialer := &net.Dialer{
				Timeout:   timeout,
				KeepAlive: 0,
			}
			start := time.Now()

			var conn net.Conn
			var err error
			if strings.Contains(ip, ":") {
				conn, err = dialer.Dial("tcp", ip)
			} else {
				conn, err = dialer.Dial("tcp", net.JoinHostPort(ip, strconv.Itoa(*defaultPort)))
			}

			if err != nil {
				return
			}
			defer conn.Close()

			tcpDuration := time.Since(start)
			start = time.Now()

			client := http.Client{
				Transport: &http.Transport{
					Dial: func(network, addr string) (net.Conn, error) {
						return conn, nil
					},
				},
				Timeout: timeout,
			}

			var protocol string
			if *enableTLS {
				protocol = "https://"
			} else {
				protocol = "http://"
			}
			requestURL := protocol + requestURL

			req, _ := http.NewRequest("GET", requestURL, nil)

			// 添加用户代理
			req.Header.Set("User-Agent", "Mozilla/5.0")
			req.Close = true
			resp, err := client.Do(req)
			if err != nil {
				return
			}

			duration := time.Since(start)
			if duration > maxDuration {
				return
			}

			buf := &bytes.Buffer{}
			// 创建一个读取操作的超时
			timeout := time.After(maxDuration)
			// 使用一个 goroutine 来读取响应体
			done := make(chan bool)
			go func() {
				_, err := io.Copy(buf, resp.Body)
				done <- true
				if err != nil {
					return
				}
			}()
			// 等待读取操作完成或者超时
			select {
			case <-done:
				// 读取操作完成
			case <-timeout:
				// 读取操作超时
				return
			}

			body := buf
			if err != nil {
				return
			}
			bodystring := body.String()

			if strings.Contains(bodystring, "uag=Mozilla/5.0") {
				if matches := regexp.MustCompile(`colo=([A-Z]+)`).FindStringSubmatch(body.String()); len(matches) > 1 {

					fmt.Printf("发现有效IP %s 延迟 %d 毫秒\n", ip, tcpDuration.Milliseconds())
					realPort := *defaultPort
					if strings.Contains(ip, ":") {
						realPort, _ = strconv.Atoi(strings.Split(ip, ":")[1])
					}
					resultChan <- result{ip, realPort, fmt.Sprintf("%d ms", tcpDuration.Milliseconds()), tcpDuration}

				}
			}
		}(ip)
	}

	wg.Wait()
	close(resultChan)

	if len(resultChan) == 0 {
		// 清除输出内容
		fmt.Print("\033[2J")
		fmt.Println("没有发现有效的IP")
		return
	}
	var results []speedtestresult
	if *speedTest > 0 {
		fmt.Printf("开始测速\n")
		var wg2 sync.WaitGroup
		wg2.Add(*speedTest)
		count = 0
		total := len(resultChan)
		results = []speedtestresult{}
		for i := 0; i < *speedTest; i++ {
			thread <- struct{}{}
			go func() {
				defer func() {
					<-thread
					wg2.Done()
				}()
				for res := range resultChan {

					downloadSpeed := getDownloadSpeed(res.ip)
					results = append(results, speedtestresult{result: res, downloadSpeed: downloadSpeed})

					count++
					percentage := float64(count) / float64(total) * 100
					fmt.Printf("已完成: %.2f%%\r", percentage)
					if count == total {
						fmt.Printf("已完成: %.2f%%\033[0\n", percentage)
					}
				}
			}()
		}
		wg2.Wait()
	} else {
		for res := range resultChan {
			results = append(results, speedtestresult{result: res})
		}
	}

	if *speedTest > 0 {
		sort.Slice(results, func(i, j int) bool {
			return results[i].downloadSpeed > results[j].downloadSpeed
		})
	} else {
		sort.Slice(results, func(i, j int) bool {
			return results[i].result.tcpDuration < results[j].result.tcpDuration
		})
	}

	file, err := os.Create(*outFile)
	if err != nil {
		fmt.Printf("无法创建文件: %v\n", err)
		return
	}
	defer file.Close()

	writer := csv.NewWriter(file)
	if *speedTest > 0 {
		writer.Write([]string{"IP地址", "端口", "TLS", "网络延迟", "下载速度"})
	} else {
		writer.Write([]string{"IP地址", "端口", "TLS", "网络延迟"})
	}
	for _, res := range results {
		if *speedTest > 0 {
			writer.Write([]string{res.result.ip, strconv.Itoa(res.result.port), strconv.FormatBool(*enableTLS), res.result.latency, fmt.Sprintf("%.0f kB/s", res.downloadSpeed)})
		} else {
			writer.Write([]string{res.result.ip, strconv.Itoa(res.result.port), strconv.FormatBool(*enableTLS), res.result.latency})
		}
	}

	writer.Flush()
	// 清除输出内容
	fmt.Print("\033[2J")
	fmt.Printf("成功将结果写入文件 %s，耗时 %d秒\n", *outFile, time.Since(startTime)/time.Second)
}

// 从文件中读取IP地址
func readIPs(File string) ([]string, error) {
	file, err := os.Open(File)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var ips []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		ipAddr := scanner.Text()
		// 判断是否为 CIDR 格式的 IP 地址
		if strings.Contains(ipAddr, "/") {
			ip, ipNet, err := net.ParseCIDR(ipAddr)
			if err != nil {
				fmt.Printf("无法解析CIDR格式的IP: %v\n", err)
				continue
			}
			for ip := ip.Mask(ipNet.Mask); ipNet.Contains(ip); inc(ip) {
				ips = append(ips, ip.String())
			}
		} else {
			ips = append(ips, ipAddr)
		}
	}
	return ips, scanner.Err()
}

// inc函数实现ip地址自增
func inc(ip net.IP) {
	for j := len(ip) - 1; j >= 0; j-- {
		ip[j]++
		if ip[j] > 0 {
			break
		}
	}
}

// 测速函数
func getDownloadSpeed(ip string) float64 {
	var protocol string
	if *enableTLS {
		protocol = "https://"
	} else {
		protocol = "http://"
	}
	speedTestURL := protocol + *speedTestURL
	// 创建请求
	req, _ := http.NewRequest("GET", speedTestURL, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")

	// 创建TCP连接
	dialer := &net.Dialer{
		Timeout:   timeout,
		KeepAlive: 0,
	}
	var conn net.Conn
	var err error
	if strings.Contains(ip, ":") {
		conn, err = dialer.Dial("tcp", ip)
	} else {
		conn, err = dialer.Dial("tcp", net.JoinHostPort(ip, strconv.Itoa(*defaultPort)))
	}
	if err != nil {
		return 0
	}
	defer conn.Close()

	fmt.Printf("正在测试IP %s 端口 %s\n", ip, strconv.Itoa(*defaultPort))
	startTime := time.Now()
	// 创建HTTP客户端
	client := http.Client{
		Transport: &http.Transport{
			Dial: func(network, addr string) (net.Conn, error) {
				return conn, nil
			},
		},
		//设置单个IP测速最长时间为5秒
		Timeout: 10 * time.Second,
	}
	// 发送请求
	req.Close = true
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("IP %s 端口 %s 测速无效\n", ip, strconv.Itoa(*defaultPort))
		return 0
	}
	defer resp.Body.Close()

	// 复制响应体到/dev/null，并计算下载速度
	written, _ := io.Copy(io.Discard, resp.Body)
	duration := time.Since(startTime)
	speed := float64(written) / duration.Seconds() / 1024

	// 输出结果
	fmt.Printf("IP %s 端口 %s 下载速度 %.0f kB/s\n", ip, strconv.Itoa(*defaultPort), speed)
	return speed
}
