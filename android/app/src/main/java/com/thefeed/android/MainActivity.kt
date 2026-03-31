package com.thefeed.android

import android.content.Intent
import android.os.Build
import android.os.Bundle
import android.webkit.WebSettings
import android.webkit.WebView
import android.webkit.WebViewClient
import android.widget.Button
import android.widget.TextView
import androidx.activity.ComponentActivity

class MainActivity : ComponentActivity() {
    private lateinit var webView: WebView
    private lateinit var txtStatus: TextView

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContentView(R.layout.activity_main)

        webView = findViewById(R.id.webView)
        txtStatus = findViewById(R.id.txtStatus)

        findViewById<Button>(R.id.btnStart).setOnClickListener {
            startThefeedService()
            txtStatus.text = "Service started. Opening local UI..."
            loadLocalWeb()
        }

        findViewById<Button>(R.id.btnStop).setOnClickListener {
            stopService(Intent(this, ThefeedService::class.java))
            txtStatus.text = "Service stopped"
        }

        findViewById<Button>(R.id.btnReload).setOnClickListener {
            loadLocalWeb()
        }

        configureWebView()

        // Start in background by default so WebView can connect on first launch.
        startThefeedService()
        loadLocalWeb()
    }

    private fun startThefeedService() {
        val intent = Intent(this, ThefeedService::class.java)
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            startForegroundService(intent)
        } else {
            startService(intent)
        }
    }

    private fun configureWebView() {
        webView.webViewClient = object : WebViewClient() {
            override fun onPageFinished(view: WebView?, url: String?) {
                txtStatus.text = "Connected to local UI"
            }
        }

        with(webView.settings) {
            javaScriptEnabled = true
            domStorageEnabled = true
            cacheMode = WebSettings.LOAD_DEFAULT
            allowFileAccess = false
            allowContentAccess = false
        }
    }

    private fun loadLocalWeb() {
        txtStatus.text = "Loading http://127.0.0.1:8080 ..."
        webView.postDelayed({ webView.loadUrl("http://127.0.0.1:8080") }, 500)
    }

    override fun onDestroy() {
        webView.destroy()
        super.onDestroy()
    }
}
