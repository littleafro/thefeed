package com.thefeed.android

import android.Manifest
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.os.Build
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.webkit.WebResourceError
import android.webkit.WebResourceRequest
import android.webkit.WebSettings
import android.webkit.WebView
import android.webkit.WebViewClient
import android.view.View
import android.widget.TextView
import androidx.activity.ComponentActivity
import androidx.activity.result.contract.ActivityResultContracts
import androidx.core.content.ContextCompat
import java.net.HttpURLConnection
import java.net.URL

class MainActivity : ComponentActivity() {
    private lateinit var webView: WebView
    private lateinit var txtStatus: TextView
    private val handler = Handler(Looper.getMainLooper())

    private val notificationPermissionLauncher = registerForActivityResult(
        ActivityResultContracts.RequestPermission()
    ) { /* granted or not, service still works */ }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContentView(R.layout.activity_main)

        webView = findViewById(R.id.webView)
        txtStatus = findViewById(R.id.txtStatus)

        requestNotificationPermission()
        configureWebView()
        startThefeedService()
        waitForServerThenLoad()
    }

    private fun requestNotificationPermission() {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
            if (ContextCompat.checkSelfPermission(this, Manifest.permission.POST_NOTIFICATIONS)
                != PackageManager.PERMISSION_GRANTED
            ) {
                notificationPermissionLauncher.launch(Manifest.permission.POST_NOTIFICATIONS)
            }
        }
    }

    private fun startThefeedService() {
        val intent = Intent(this, ThefeedService::class.java)
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            startForegroundService(intent)
        } else {
            startService(intent)
        }
    }

    private fun setStatus(msg: String) {
        txtStatus.text = msg
        txtStatus.visibility = if (msg.isEmpty()) View.GONE else View.VISIBLE
    }

    private fun configureWebView() {
        webView.webViewClient = object : WebViewClient() {
            override fun onPageFinished(view: WebView?, url: String?) {
                if (url != null && url.startsWith("http://127.0.0.1")) {
                    setStatus("")
                }
            }

            override fun onReceivedError(
                view: WebView?,
                request: WebResourceRequest?,
                error: WebResourceError?
            ) {
                // Server was reachable during probe but dropped connection — retry probe cycle
                if (request?.isForMainFrame == true) {
                    setStatus("Reconnecting...")
                    handler.postDelayed({ waitForServerThenLoad() }, RETRY_DELAY_MS)
                }
            }
        }

        with(webView.settings) {
            javaScriptEnabled = true
            domStorageEnabled = true
            cacheMode = WebSettings.LOAD_NO_CACHE
            allowFileAccess = false
            allowContentAccess = false
            mixedContentMode = WebSettings.MIXED_CONTENT_NEVER_ALLOW
        }
    }

    /**
     * Polls SharedPreferences for the port on every attempt, then probes the URL.
     * This handles force-kill restarts where the service picks a new port:
     * the loop follows the port change automatically instead of hammering a stale one.
     */
    private fun waitForServerThenLoad() {
        setStatus("Waiting for service...")
        Thread {
            var ready = false
            var lastPort = -1
            for (attempt in 1..MAX_PROBE_ATTEMPTS) {
                val port = getCurrentPort()
                if (port <= 0) {
                    handler.post { setStatus("Waiting for service... ($attempt/$MAX_PROBE_ATTEMPTS)") }
                    Thread.sleep(PROBE_INTERVAL_MS)
                    continue
                }
                if (port != lastPort) {
                    // Service restarted with a new port — reset and start fresh
                    lastPort = port
                    handler.post { setStatus("Connecting...") }
                }
                try {
                    val conn = URL("http://127.0.0.1:$port").openConnection() as HttpURLConnection
                    conn.connectTimeout = PROBE_TIMEOUT_MS.toInt()
                    conn.readTimeout = PROBE_TIMEOUT_MS.toInt()
                    conn.requestMethod = "GET"
                    val code = conn.responseCode
                    conn.disconnect()
                    if (code > 0) {
                        ready = true
                        val url = "http://127.0.0.1:$port"
                        handler.post { setStatus(""); webView.loadUrl(url) }
                        return@Thread
                    }
                } catch (_: Exception) {
                    // Connection refused — not ready yet
                }
                handler.post { setStatus("Waiting for server... ($attempt/$MAX_PROBE_ATTEMPTS)") }
                Thread.sleep(PROBE_INTERVAL_MS)
            }
            if (!ready) {
                handler.post { setStatus("Could not connect. Restart the app.") }
            }
        }.start()
    }

    private fun getCurrentPort(): Int {
        val prefs = getSharedPreferences(ThefeedService.PREFS_NAME, Context.MODE_PRIVATE)
        return prefs.getInt(ThefeedService.PREF_PORT, -1)
    }

    override fun onDestroy() {
        handler.removeCallbacksAndMessages(null)
        webView.destroy()
        super.onDestroy()
    }

    companion object {
        private const val MAX_PROBE_ATTEMPTS = 30
        private const val PROBE_INTERVAL_MS = 1000L   // 1s between probes → up to 30s total
        private const val PROBE_TIMEOUT_MS  = 1000L   // 1s HTTP connect timeout per probe
        private const val RETRY_DELAY_MS    = 2000L   // delay before restarting probe cycle on error
    }
}

