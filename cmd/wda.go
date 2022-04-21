/*
 *  Copyright (C) [SonicCloudOrg] Sonic Project
 *
 *  Licensed under the Apache License, Version 2.0 (the "License");
 *  you may not use this file except in compliance with the License.
 *  You may obtain a copy of the License at
 *
 *         http://www.apache.org/licenses/LICENSE-2.0
 *
 *  Unless required by applicable law or agreed to in writing, software
 *  distributed under the License is distributed on an "AS IS" BASIS,
 *  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 *  See the License for the specific language governing permissions and
 *  limitations under the License.
 *
 */
package cmd

import (
	"fmt"
	"github.com/SonicCloudOrg/sonic-ios-bridge/src/entity"
	"github.com/SonicCloudOrg/sonic-ios-bridge/src/util"
	giDevice "github.com/electricbubble/gidevice"
	"github.com/mitchellh/mapstructure"
	"github.com/spf13/cobra"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

var wdaCmd = &cobra.Command{
	Use:   "wda",
	Short: "Run WebDriverAgent on your devices",
	Long:  `Run WebDriverAgent on your devices`,
	RunE: func(cmd *cobra.Command, args []string) error {
		usbMuxClient, err := giDevice.NewUsbmux()
		if err != nil {
			return util.NewErrorPrint(util.ErrConnect, "usbMux", err)
		}
		list, err1 := usbMuxClient.Devices()
		if err1 != nil {
			return util.NewErrorPrint(util.ErrSendCommand, "listDevices", err1)
		}
		if len(list) == 0 {
			fmt.Println("no device connected")
			os.Exit(0)
		} else {
			var device giDevice.Device
			if len(udid) != 0 {
				for i, d := range list {
					if d.Properties().SerialNumber == udid {
						device = list[i]
						break
					}
				}
			} else {
				device = list[0]
			}
			if device.Properties().SerialNumber != "" {
				if !strings.HasSuffix(wdaBundleID, ".xctrunner") {
					wdaBundleID += ".xctrunner"
				}
				appList, errList := device.InstallationProxyBrowse(giDevice.WithApplicationType(giDevice.ApplicationTypeUser))
				if errList != nil {
					return util.NewErrorPrint(util.ErrSendCommand, "appList", errList)
				}
				var hasWda = false
				for _, d := range appList {
					a := entity.Application{}
					mapstructure.Decode(d, &a)
					if a.CFBundleIdentifier == wdaBundleID {
						hasWda = true
						break
					}
				}
				if !hasWda {
					fmt.Printf("%s is not in your device!", wdaBundleID)
					os.Exit(0)
				}
				testEnv := make(map[string]interface{})
				testEnv["USE_PORT"] = serverRemotePort
				testEnv["MJPEG_SERVER_PORT"] = mjpegRemotePort
				util.CheckMount(device)
				output, stopTest, err2 := device.XCTest(wdaBundleID, giDevice.WithXCTestEnv(testEnv))
				if err2 != nil {
					fmt.Printf("WebDriverAgent server start failed: %s", err2)
					os.Exit(0)
				}
				serverListener, err := net.Listen("tcp", fmt.Sprintf(":%d", serverLocalPort))
				if err != nil {
					return err
				}
				defer serverListener.Close()
				mjpegListener, err := net.Listen("tcp", fmt.Sprintf(":%d", mjpegLocalPort))
				if err != nil {
					return err
				}
				defer mjpegListener.Close()
				shutWdaDown := make(chan os.Signal, syscall.SIGTERM)
				signal.Notify(shutWdaDown, os.Interrupt, os.Kill)

				go proxy()(serverListener, serverRemotePort, device)
				go proxy()(mjpegListener, mjpegRemotePort, device)

				go func() {
					for s := range output {
						fmt.Print(s)
						if strings.Contains(s, "ServerURLHere->") {
							fmt.Println("WebDriverAgent server start successful")
						}
					}
					shutWdaDown <- os.Interrupt
				}()

				go func() {
					var resp *http.Response
					var httpErr error
					var checkTime = 0
					for {
						time.Sleep(time.Duration(30) * time.Second)
						checkTime++
						resp, httpErr = http.Get(fmt.Sprintf("http://127.0.0.1:%d/status", serverLocalPort))
						if httpErr != nil {
							fmt.Printf("request fail: %s", httpErr)
							continue
						}
						if resp.StatusCode == 200 {
							fmt.Printf("wda server health checked %d times: ok", checkTime)
						} else {
							stopTest()
							var upTimes = 0
							for {
								output, stopTest, err2 = device.XCTest(wdaBundleID, giDevice.WithXCTestEnv(testEnv))
								upTimes++
								if err2 != nil {
									fmt.Printf("WebDriverAgent server start failed in %d times: %s", upTimes, err2)
									if upTimes >= 3 {
										fmt.Printf("WebDriverAgent server start failed more than 3 times, giving up...")
										os.Exit(0)
									}
								} else {
									break
								}
							}
						}
					}
					defer resp.Body.Close()
				}()

				<-shutWdaDown
				stopTest()
				fmt.Println("stopped")
			} else {
				fmt.Println("device no found")
				os.Exit(0)
			}
		}
		return nil
	},
}

var (
	wdaBundleID      string
	serverRemotePort int
	mjpegRemotePort  int
	serverLocalPort  int
	mjpegLocalPort   int
)

func init() {
	runCmd.AddCommand(wdaCmd)
	wdaCmd.Flags().StringVarP(&udid, "udid", "u", "", "device's serialNumber ( default first device )")
	wdaCmd.Flags().StringVarP(&wdaBundleID, "bundleId", "b", "com.facebook.WebDriverAgentRunner.xctrunner", "WebDriverAgentRunner bundleId")
	wdaCmd.Flags().IntVarP(&serverRemotePort, "server-remote-port", "", 8100, "WebDriverAgentRunner server remote port")
	wdaCmd.Flags().IntVarP(&mjpegRemotePort, "mjpeg-remote-port", "", 9100, "mjpeg-server remote port")
	wdaCmd.Flags().IntVarP(&serverLocalPort, "server-local-port", "", 8100, "WebDriverAgentRunner server local port")
	wdaCmd.Flags().IntVarP(&mjpegLocalPort, "mjpeg-local-port", "", 9100, "mjpeg-server local port")
}

func proxy() func(listener net.Listener, port int, device giDevice.Device) {
	return func(listener net.Listener, port int, device giDevice.Device) {
		for {
			var accept net.Conn
			var err error
			if accept, err = listener.Accept(); err != nil {
				log.Println("accept:", err)
			}
			fmt.Println("accept", accept.RemoteAddr())
			rInnerConn, err := device.NewConnect(port)
			if err != nil {
				fmt.Println("connect to device fail")
				os.Exit(0)
			}
			rConn := rInnerConn.RawConn()
			_ = rConn.SetDeadline(time.Time{})
			go func(lConn net.Conn) {
				go func(lConn, rConn net.Conn) {
					if _, err := io.Copy(lConn, rConn); err != nil {
						log.Println("copy local -> remote failed:", err)
					}
				}(lConn, rConn)
				go func(lConn, rConn net.Conn) {
					if _, err := io.Copy(rConn, lConn); err != nil {
						log.Println("copy local <- remote failed:", err)
					}
				}(lConn, rConn)
			}(accept)
		}
	}
}
